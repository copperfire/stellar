package plugins

import (
	"fmt"
	"log"

	"github.com/interstellar/kelp/api"
	"github.com/interstellar/kelp/model"
	"github.com/interstellar/kelp/support/utils"
	"github.com/stellar/go/build"
	"github.com/stellar/go/clients/horizon"
)

type exchangeAPIKeysToml []struct {
	Key    string `valid:"-" toml:"KEY"`
	Secret string `valid:"-" toml:"SECRET"`
}

func (t *exchangeAPIKeysToml) toExchangeAPIKeys() []api.ExchangeAPIKey {
	apiKeys := []api.ExchangeAPIKey{}
	for _, apiKey := range *t {
		apiKeys = append(apiKeys, api.ExchangeAPIKey{
			Key:    apiKey.Key,
			Secret: apiKey.Secret,
		})
	}
	return apiKeys
}

// mirrorConfig contains the configuration params for this strategy
type mirrorConfig struct {
	Exchange        string              `valid:"-" toml:"EXCHANGE"`
	ExchangeBase    string              `valid:"-" toml:"EXCHANGE_BASE"`
	ExchangeQuote   string              `valid:"-" toml:"EXCHANGE_QUOTE"`
	OrderbookDepth  int32               `valid:"-" toml:"ORDERBOOK_DEPTH"`
	VolumeDivideBy  float64             `valid:"-" toml:"VOLUME_DIVIDE_BY"`
	PerLevelSpread  float64             `valid:"-" toml:"PER_LEVEL_SPREAD"`
	OffsetTrades    bool                `valid:"-" toml:"OFFSET_TRADES"`
	ExchangeAPIKeys exchangeAPIKeysToml `valid:"-" toml:"EXCHANGE_API_KEYS"`
}

// String impl.
func (c mirrorConfig) String() string {
	return utils.StructString(c, nil)
}

// mirrorStrategy is a strategy to mirror the orderbook of a given exchange
type mirrorStrategy struct {
	sdex               *SDEX
	baseAsset          *horizon.Asset
	quoteAsset         *horizon.Asset
	primaryConstraints *model.OrderConstraints
	backingPair        *model.TradingPair
	backingConstraints *model.OrderConstraints
	orderbookDepth     int32
	perLevelSpread     float64
	volumeDivideBy     float64
	tradeAPI           api.TradeAPI
	offsetTrades       bool
}

// ensure this implements api.Strategy
var _ api.Strategy = &mirrorStrategy{}

// ensure this implements api.FillHandler
var _ api.FillHandler = &mirrorStrategy{}

// makeMirrorStrategy is a factory method
func makeMirrorStrategy(sdex *SDEX, pair *model.TradingPair, baseAsset *horizon.Asset, quoteAsset *horizon.Asset, config *mirrorConfig, simMode bool) (api.Strategy, error) {
	var exchange api.Exchange
	var e error
	if config.OffsetTrades {
		exchangeAPIKeys := config.ExchangeAPIKeys.toExchangeAPIKeys()
		exchange, e = MakeTradingExchange(config.Exchange, exchangeAPIKeys, simMode)
		if e != nil {
			return nil, e
		}
	} else {
		exchange, e = MakeExchange(config.Exchange, simMode)
		if e != nil {
			return nil, e
		}
	}

	// we have two sets of (tradingPair, orderConstraints): the primaryExchange and the backingExchange
	primaryConstraints := sdex.GetOrderConstraints(pair)
	// backingPair is taken from the mirror strategy config not from the passed in trading pair
	backingPair := &model.TradingPair{
		Base:  exchange.GetAssetConverter().MustFromString(config.ExchangeBase),
		Quote: exchange.GetAssetConverter().MustFromString(config.ExchangeQuote),
	}
	backingConstraints := exchange.GetOrderConstraints(backingPair)
	return &mirrorStrategy{
		sdex:               sdex,
		baseAsset:          baseAsset,
		quoteAsset:         quoteAsset,
		primaryConstraints: primaryConstraints,
		backingPair:        backingPair,
		backingConstraints: backingConstraints,
		orderbookDepth:     config.OrderbookDepth,
		perLevelSpread:     config.PerLevelSpread,
		volumeDivideBy:     config.VolumeDivideBy,
		tradeAPI:           api.TradeAPI(exchange),
		offsetTrades:       config.OffsetTrades,
	}, nil
}

// PruneExistingOffers deletes any extra offers
func (s mirrorStrategy) PruneExistingOffers(buyingAOffers []horizon.Offer, sellingAOffers []horizon.Offer) ([]build.TransactionMutator, []horizon.Offer, []horizon.Offer) {
	return []build.TransactionMutator{}, buyingAOffers, sellingAOffers
}

// PreUpdate changes the strategy's state in prepration for the update
func (s *mirrorStrategy) PreUpdate(maxAssetA float64, maxAssetB float64, trustA float64, trustB float64) error {
	return nil
}

// UpdateWithOps builds the operations we want performed on the account
func (s mirrorStrategy) UpdateWithOps(
	buyingAOffers []horizon.Offer,
	sellingAOffers []horizon.Offer,
) ([]build.TransactionMutator, error) {
	ob, e := s.tradeAPI.GetOrderBook(s.backingPair, s.orderbookDepth)
	if e != nil {
		return nil, e
	}

	// limit bids and asks to max 50 operations each because of Stellar's limit of 100 ops/tx
	bids := ob.Bids()
	if len(bids) > 50 {
		bids = bids[:50]
	}
	asks := ob.Asks()
	if len(asks) > 50 {
		asks = asks[:50]
	}

	buyOps, e := s.updateLevels(
		buyingAOffers,
		bids,
		s.sdex.ModifyBuyOffer,
		s.sdex.CreateBuyOffer,
		(1 - s.perLevelSpread),
		true,
	)
	if e != nil {
		return nil, e
	}
	log.Printf("num. buyOps in this update: %d\n", len(buyOps))

	sellOps, e := s.updateLevels(
		sellingAOffers,
		asks,
		s.sdex.ModifySellOffer,
		s.sdex.CreateSellOffer,
		(1 + s.perLevelSpread),
		false,
	)
	if e != nil {
		return nil, e
	}
	log.Printf("num. sellOps in this update: %d\n", len(sellOps))

	ops := []build.TransactionMutator{}
	if len(ob.Bids()) > 0 && len(sellingAOffers) > 0 && ob.Bids()[0].Price.AsFloat() >= utils.PriceAsFloat(sellingAOffers[0].Price) {
		ops = append(ops, sellOps...)
		ops = append(ops, buyOps...)
	} else {
		ops = append(ops, buyOps...)
		ops = append(ops, sellOps...)
	}

	return ops, nil
}

func (s *mirrorStrategy) updateLevels(
	oldOffers []horizon.Offer,
	newOrders []model.Order,
	modifyOffer func(offer horizon.Offer, price float64, amount float64, incrementalNativeAmountRaw float64) (*build.ManageOfferBuilder, error),
	createOffer func(baseAsset horizon.Asset, quoteAsset horizon.Asset, price float64, amount float64, incrementalNativeAmountRaw float64) (*build.ManageOfferBuilder, error),
	priceMultiplier float64,
	hackPriceInvertForBuyOrderChangeCheck bool, // needed because createBuy and modBuy inverts price so we need this for price comparison in doModifyOffer
) ([]build.TransactionMutator, error) {
	ops := []build.TransactionMutator{}
	deleteOps := []build.TransactionMutator{}
	if len(newOrders) >= len(oldOffers) {
		for i := 0; i < len(oldOffers); i++ {
			modifyOp, deleteOp, e := s.doModifyOffer(oldOffers[i], newOrders[i], priceMultiplier, modifyOffer, hackPriceInvertForBuyOrderChangeCheck)
			if e != nil {
				return nil, e
			}
			if modifyOp != nil {
				ops = append(ops, modifyOp)
			}
			if deleteOp != nil {
				deleteOps = append(deleteOps, deleteOp)
			}
		}

		// create offers for remaining new bids
		for i := len(oldOffers); i < len(newOrders); i++ {
			price := newOrders[i].Price.Scale(priceMultiplier).AsFloat()
			vol := newOrders[i].Volume.Scale(1.0 / s.volumeDivideBy).AsFloat()
			incrementalNativeAmountRaw := s.sdex.ComputeIncrementalNativeAmountRaw(true)
			mo, e := createOffer(*s.baseAsset, *s.quoteAsset, price, vol, incrementalNativeAmountRaw)
			if e != nil {
				return nil, e
			}
			if mo != nil {
				ops = append(ops, *mo)
				// update the cached liabilities if we create a valid operation to create an offer
				if hackPriceInvertForBuyOrderChangeCheck {
					s.sdex.AddLiabilities(*s.quoteAsset, *s.baseAsset, vol*price, vol, incrementalNativeAmountRaw)
				} else {
					s.sdex.AddLiabilities(*s.baseAsset, *s.quoteAsset, vol, vol*price, incrementalNativeAmountRaw)
				}
			}
		}
	} else {
		for i := 0; i < len(newOrders); i++ {
			modifyOp, deleteOp, e := s.doModifyOffer(oldOffers[i], newOrders[i], priceMultiplier, modifyOffer, hackPriceInvertForBuyOrderChangeCheck)
			if e != nil {
				return nil, e
			}
			if modifyOp != nil {
				ops = append(ops, modifyOp)
			}
			if deleteOp != nil {
				deleteOps = append(deleteOps, deleteOp)
			}
		}

		// delete remaining prior offers
		for i := len(newOrders); i < len(oldOffers); i++ {
			deleteOp := s.sdex.DeleteOffer(oldOffers[i])
			deleteOps = append(deleteOps, deleteOp)
		}
	}

	// prepend deleteOps because we want to delete offers first so we "free" up our liabilities capacity to place the new/modified offers
	allOps := append(deleteOps, ops...)
	log.Printf("prepended %d deleteOps\n", len(deleteOps))

	return allOps, nil
}

// doModifyOffer returns a new modifyOp, deleteOp, error
func (s *mirrorStrategy) doModifyOffer(
	oldOffer horizon.Offer,
	newOrder model.Order,
	priceMultiplier float64,
	modifyOffer func(offer horizon.Offer, price float64, amount float64, incrementalNativeAmountRaw float64) (*build.ManageOfferBuilder, error),
	hackPriceInvertForBuyOrderChangeCheck bool, // needed because createBuy and modBuy inverts price so we need this for price comparison in doModifyOffer
) (build.TransactionMutator, build.TransactionMutator, error) {
	price := newOrder.Price.Scale(priceMultiplier)
	vol := newOrder.Volume.Scale(1.0 / s.volumeDivideBy)
	oldPrice := model.MustNumberFromString(oldOffer.Price, s.primaryConstraints.PricePrecision)
	oldVol := model.MustNumberFromString(oldOffer.Amount, s.primaryConstraints.VolumePrecision)
	if hackPriceInvertForBuyOrderChangeCheck {
		// we want to multiply oldVol by the original oldPrice so we can get the correct oldVol, since ModifyBuyOffer multiplies price * vol
		oldVol = oldVol.Multiply(*oldPrice)
		oldPrice = model.InvertNumber(oldPrice)
	}
	newPrice := price
	newVol := vol
	epsilon := 0.0001
	incrementalNativeAmountRaw := s.sdex.ComputeIncrementalNativeAmountRaw(false)
	sameOrderParams := oldPrice.EqualsPrecisionNormalized(*newPrice, epsilon) && oldVol.EqualsPrecisionNormalized(*newVol, epsilon)
	if sameOrderParams {
		// update the cached liabilities if we keep the existing offer
		if hackPriceInvertForBuyOrderChangeCheck {
			s.sdex.AddLiabilities(oldOffer.Selling, oldOffer.Buying, oldVol.Multiply(*oldPrice).AsFloat(), oldVol.AsFloat(), incrementalNativeAmountRaw)
		} else {
			s.sdex.AddLiabilities(oldOffer.Selling, oldOffer.Buying, oldVol.AsFloat(), oldVol.Multiply(*oldPrice).AsFloat(), incrementalNativeAmountRaw)
		}
		return nil, nil, nil
	}

	// convert the precision from the backing exchange to the primary exchange
	offerPrice := model.NumberByCappingPrecision(price, s.primaryConstraints.PricePrecision)
	offerAmount := model.NumberByCappingPrecision(vol, s.primaryConstraints.VolumePrecision)
	mo, e := modifyOffer(
		oldOffer,
		offerPrice.AsFloat(),
		offerAmount.AsFloat(),
		incrementalNativeAmountRaw,
	)
	if e != nil {
		return nil, nil, e
	}
	if mo != nil {
		// update the cached liabilities if we create a valid operation to modify the offer
		if hackPriceInvertForBuyOrderChangeCheck {
			s.sdex.AddLiabilities(oldOffer.Selling, oldOffer.Buying, offerAmount.Multiply(*offerPrice).AsFloat(), offerAmount.AsFloat(), incrementalNativeAmountRaw)
		} else {
			s.sdex.AddLiabilities(oldOffer.Selling, oldOffer.Buying, offerAmount.AsFloat(), offerAmount.Multiply(*offerPrice).AsFloat(), incrementalNativeAmountRaw)
		}
		return *mo, nil, nil
	}

	// since mo is nil we want to delete this offer
	deleteOp := s.sdex.DeleteOffer(oldOffer)
	return nil, deleteOp, nil
}

// PostUpdate changes the strategy's state after the update has taken place
func (s *mirrorStrategy) PostUpdate() error {
	return nil
}

// GetFillHandlers impl
func (s *mirrorStrategy) GetFillHandlers() ([]api.FillHandler, error) {
	if s.offsetTrades {
		return []api.FillHandler{s}, nil
	}
	return nil, nil
}

// HandleFill impl
func (s *mirrorStrategy) HandleFill(trade model.Trade) error {
	newOrder := model.Order{
		Pair:        s.backingPair, // we want to offset trades on the backing exchange so use the backing exchange's trading pair
		OrderAction: trade.OrderAction.Reverse(),
		OrderType:   model.OrderTypeLimit,
		Price:       model.NumberByCappingPrecision(trade.Price, s.backingConstraints.PricePrecision),
		Volume:      model.NumberByCappingPrecision(trade.Volume, s.backingConstraints.VolumePrecision),
		Timestamp:   nil,
	}

	log.Printf("mirror strategy is going to offset the trade from the primary exchange (transactionID=%s) onto the backing exchange with the order: %s\n", trade.TransactionID, newOrder)
	transactionID, e := s.tradeAPI.AddOrder(&newOrder)
	if e != nil {
		return fmt.Errorf("error when offsetting trade (%s): %s", newOrder, e)
	}

	log.Printf("...mirror strategy successfully offset the trade from the primary exchange (transactionID=%s) onto the backing exchange (transactionID=%s) with the order %s\n", trade.TransactionID, transactionID, newOrder)
	return nil
}
