package plugins

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	hProtocol "github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/kelp/model"
	"github.com/stellar/kelp/queries"
	"github.com/stellar/kelp/support/postgresdb"
	"github.com/stellar/kelp/support/utils"
)

type volumeFilterMode string

// type of volumeFilterMode
const (
	volumeFilterModeExact  volumeFilterMode = "exact"
	volumeFilterModeIgnore volumeFilterMode = "ignore"
)

func parseVolumeFilterMode(mode string) (volumeFilterMode, error) {
	if mode == string(volumeFilterModeExact) {
		return volumeFilterModeExact, nil
	} else if mode == string(volumeFilterModeIgnore) {
		return volumeFilterModeIgnore, nil
	}
	return volumeFilterModeExact, fmt.Errorf("invalid input mode '%s'", mode)
}

// VolumeFilterConfig ensures that any one constraint that is hit will result in deleting all offers and pausing until limits are no longer constrained
type VolumeFilterConfig struct {
	SellBaseAssetCapInBaseUnits  *float64
	SellBaseAssetCapInQuoteUnits *float64
	mode                         volumeFilterMode
	additionalMarketIDs          []string
	// buyBaseAssetCapInBaseUnits   *float64
	// buyBaseAssetCapInQuoteUnits  *float64
}

type volumeFilter struct {
	name                   string
	baseAsset              hProtocol.Asset
	quoteAsset             hProtocol.Asset
	config                 *VolumeFilterConfig
	dailyVolumeByDateQuery *queries.DailyVolumeByDate
}

// makeFilterVolume makes a submit filter that limits orders placed based on the daily volume traded
func makeFilterVolume(
	exchangeName string,
	tradingPair *model.TradingPair,
	assetDisplayFn model.AssetDisplayFn,
	baseAsset hProtocol.Asset,
	quoteAsset hProtocol.Asset,
	db *sql.DB,
	config *VolumeFilterConfig,
) (SubmitFilter, error) {
	// use assetDisplayFn to make baseAssetString and quoteAssetString because it is issuer independent for non-sdex exchanges keeping a consistent marketID
	baseAssetString, e := assetDisplayFn(tradingPair.Base)
	if e != nil {
		return nil, fmt.Errorf("could not convert base asset (%s) from trading pair via the passed in assetDisplayFn: %s", string(tradingPair.Base), e)
	}
	quoteAssetString, e := assetDisplayFn(tradingPair.Quote)
	if e != nil {
		return nil, fmt.Errorf("could not convert quote asset (%s) from trading pair via the passed in assetDisplayFn: %s", string(tradingPair.Quote), e)
	}
	marketID := makeMarketID(exchangeName, baseAssetString, quoteAssetString)
	marketIDs := utils.Dedupe(append([]string{marketID}, config.additionalMarketIDs...))
	dailyVolumeByDateQuery, e := queries.MakeDailyVolumeByDateForMarketIdsAction(db, marketIDs, "sell")
	if e != nil {
		return nil, fmt.Errorf("could not make daily volume by date Query: %s", e)
	}

	return &volumeFilter{
		name:                   "volumeFilter",
		baseAsset:              baseAsset,
		quoteAsset:             quoteAsset,
		config:                 config,
		dailyVolumeByDateQuery: dailyVolumeByDateQuery,
	}, nil
}

var _ SubmitFilter = &volumeFilter{}

// Validate ensures validity
func (c *VolumeFilterConfig) Validate() error {
	if c.isEmpty() {
		return fmt.Errorf("the volumeFilterConfig was empty")
	}
	return nil
}

// String is the stringer method
func (c *VolumeFilterConfig) String() string {
	return fmt.Sprintf("VolumeFilterConfig[SellBaseAssetCapInBaseUnits=%s, SellBaseAssetCapInQuoteUnits=%s, mode=%s, additionalMarketIDs=%v]",
		utils.CheckedFloatPtr(c.SellBaseAssetCapInBaseUnits), utils.CheckedFloatPtr(c.SellBaseAssetCapInQuoteUnits), c.mode, c.additionalMarketIDs)
}

func (f *volumeFilter) Apply(ops []txnbuild.Operation, sellingOffers []hProtocol.Offer, buyingOffers []hProtocol.Offer) ([]txnbuild.Operation, error) {
	dateString := time.Now().UTC().Format(postgresdb.DateFormatString)
	// TODO do for buying base and also for flipped marketIDs
	queryResult, e := f.dailyVolumeByDateQuery.QueryRow(dateString)
	if e != nil {
		return nil, fmt.Errorf("could not load dailyValuesByDate for today (%s): %s", dateString, e)
	}
	dailyValuesBaseSold, ok := queryResult.(*queries.DailyVolume)
	if !ok {
		return nil, fmt.Errorf("incorrect type returned from DailyVolumeByDate query, expecting '*queries.DailyVolume' but was '%T'", queryResult)
	}

	log.Printf("dailyValuesByDate for today (%s): baseSoldUnits = %.8f %s, quoteCostUnits = %.8f %s (%s)\n",
		dateString, dailyValuesBaseSold.BaseVol, utils.Asset2String(f.baseAsset), dailyValuesBaseSold.QuoteVol, utils.Asset2String(f.quoteAsset), f.config)

	// daily on-the-books
	dailyOTB := &VolumeFilterConfig{
		SellBaseAssetCapInBaseUnits:  &dailyValuesBaseSold.BaseVol,
		SellBaseAssetCapInQuoteUnits: &dailyValuesBaseSold.QuoteVol,
	}
	// daily to-be-booked starts out as empty and accumulates the values of the operations
	dailyTbbSellBase := 0.0
	dailyTbbSellQuote := 0.0
	dailyTBB := &VolumeFilterConfig{
		SellBaseAssetCapInBaseUnits:  &dailyTbbSellBase,
		SellBaseAssetCapInQuoteUnits: &dailyTbbSellQuote,
	}

	innerFn := func(op *txnbuild.ManageSellOffer) (*txnbuild.ManageSellOffer, error) {
		return f.volumeFilterFn(dailyOTB, dailyTBB, op)
	}
	ops, e = filterOps(f.name, f.baseAsset, f.quoteAsset, sellingOffers, buyingOffers, ops, innerFn)
	if e != nil {
		return nil, fmt.Errorf("could not apply filter: %s", e)
	}
	return ops, nil
}

func (f *volumeFilter) volumeFilterFn(dailyOTB *VolumeFilterConfig, dailyTBB *VolumeFilterConfig, op *txnbuild.ManageSellOffer) (*txnbuild.ManageSellOffer, error) {
	isSell, e := utils.IsSelling(f.baseAsset, f.quoteAsset, op.Selling, op.Buying)
	if e != nil {
		return nil, fmt.Errorf("error when running the isSelling check for offer '%+v': %s", *op, e)
	}

	sellPrice, e := strconv.ParseFloat(op.Price, 64)
	if e != nil {
		return nil, fmt.Errorf("could not convert price (%s) to float: %s", op.Price, e)
	}

	amountValueUnitsBeingSold, e := strconv.ParseFloat(op.Amount, 64)
	if e != nil {
		return nil, fmt.Errorf("could not convert amount (%s) to float: %s", op.Amount, e)
	}

	if isSell {
		opToReturn := op
		newAmountBeingSold := amountValueUnitsBeingSold
		var keepSellingBase bool
		var keepSellingQuote bool
		if f.config.SellBaseAssetCapInBaseUnits != nil {
			projectedSoldInBaseUnits := *dailyOTB.SellBaseAssetCapInBaseUnits + *dailyTBB.SellBaseAssetCapInBaseUnits + amountValueUnitsBeingSold
			keepSellingBase = projectedSoldInBaseUnits <= *f.config.SellBaseAssetCapInBaseUnits
			newAmountString := ""
			if f.config.mode == volumeFilterModeExact && !keepSellingBase {
				newAmount := *f.config.SellBaseAssetCapInBaseUnits - *dailyOTB.SellBaseAssetCapInBaseUnits - *dailyTBB.SellBaseAssetCapInBaseUnits
				if newAmount > 0 {
					newAmountBeingSold = newAmount
					opToReturn.Amount = fmt.Sprintf("%.7f", newAmountBeingSold)
					keepSellingBase = true
					newAmountString = ", newAmountString = " + opToReturn.Amount
				}
			}
			log.Printf("volumeFilter:  selling (base units), price=%.8f amount=%.8f, keep = (projectedSoldInBaseUnits) %.7f <= %.7f (config.SellBaseAssetCapInBaseUnits): keepSellingBase = %v%s", sellPrice, amountValueUnitsBeingSold, projectedSoldInBaseUnits, *f.config.SellBaseAssetCapInBaseUnits, keepSellingBase, newAmountString)
		} else {
			keepSellingBase = true
		}

		if f.config.SellBaseAssetCapInQuoteUnits != nil {
			projectedSoldInQuoteUnits := *dailyOTB.SellBaseAssetCapInQuoteUnits + *dailyTBB.SellBaseAssetCapInQuoteUnits + (newAmountBeingSold * sellPrice)
			keepSellingQuote = projectedSoldInQuoteUnits <= *f.config.SellBaseAssetCapInQuoteUnits
			newAmountString := ""
			if f.config.mode == volumeFilterModeExact && !keepSellingQuote {
				newAmount := (*f.config.SellBaseAssetCapInQuoteUnits - *dailyOTB.SellBaseAssetCapInQuoteUnits - *dailyTBB.SellBaseAssetCapInQuoteUnits) / sellPrice
				if newAmount > 0 {
					newAmountBeingSold = newAmount
					opToReturn.Amount = fmt.Sprintf("%.7f", newAmountBeingSold)
					keepSellingQuote = true
					newAmountString = ", newAmountString = " + opToReturn.Amount
				}
			}
			log.Printf("volumeFilter: selling (quote units), price=%.8f amount=%.8f, keep = (projectedSoldInQuoteUnits) %.7f <= %.7f (config.SellBaseAssetCapInQuoteUnits): keepSellingQuote = %v%s", sellPrice, amountValueUnitsBeingSold, projectedSoldInQuoteUnits, *f.config.SellBaseAssetCapInQuoteUnits, keepSellingQuote, newAmountString)
		} else {
			keepSellingQuote = true
		}

		if keepSellingBase && keepSellingQuote {
			// update the dailyTBB to include the additional amounts so they can be used in the calculation of the next operation
			*dailyTBB.SellBaseAssetCapInBaseUnits += newAmountBeingSold
			*dailyTBB.SellBaseAssetCapInQuoteUnits += (newAmountBeingSold * sellPrice)
			return opToReturn, nil
		}
	} else {
		// TODO buying side
	}

	// we don't want to keep it so return the dropped command
	return nil, nil
}

func (c *VolumeFilterConfig) isEmpty() bool {
	if c.SellBaseAssetCapInBaseUnits != nil {
		return false
	}
	if c.SellBaseAssetCapInQuoteUnits != nil {
		return false
	}
	// if buyBaseAssetCapInBaseUnits != nil {
	// 	return false
	// }
	// if buyBaseAssetCapInQuoteUnits != nil {
	// 	return false
	// }
	return true
}
