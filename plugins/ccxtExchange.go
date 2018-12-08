package plugins

import (
	"fmt"

	"github.com/interstellar/kelp/api"
	"github.com/interstellar/kelp/model"
	"github.com/interstellar/kelp/support/sdk"
	"github.com/interstellar/kelp/support/utils"
)

// ensure that ccxtExchange conforms to the Exchange interface
var _ api.Exchange = ccxtExchange{}

// ccxtExchange is the implementation for the CCXT REST library that supports many exchanges (https://github.com/franz-see/ccxt-rest, https://github.com/ccxt/ccxt/)
type ccxtExchange struct {
	assetConverter *model.AssetConverter
	delimiter      string
	api            *sdk.Ccxt
	precision      int8
	simMode        bool
}

// makeCcxtExchange is a factory method to make an exchange using the CCXT interface
func makeCcxtExchange(ccxtBaseURL string, exchangeName string, apiKeys []api.ExchangeAPIKey, simMode bool) (api.Exchange, error) {
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("need at least 1 ExchangeAPIKey, even if it is an empty key")
	}

	c, e := sdk.MakeInitializedCcxtExchange(ccxtBaseURL, exchangeName, apiKeys[0])
	if e != nil {
		return nil, fmt.Errorf("error making a ccxt exchange: %s", e)
	}

	return ccxtExchange{
		assetConverter: model.CcxtAssetConverter,
		delimiter:      "/",
		api:            c,
		precision:      utils.SdexPrecision,
		simMode:        simMode,
	}, nil
}

// GetTickerPrice impl.
func (c ccxtExchange) GetTickerPrice(pairs []model.TradingPair) (map[model.TradingPair]api.Ticker, error) {
	pairsMap, e := model.TradingPairs2Strings(c.assetConverter, c.delimiter, pairs)
	if e != nil {
		return nil, e
	}

	priceResult := map[model.TradingPair]api.Ticker{}
	for _, p := range pairs {
		tickerMap, e := c.api.FetchTicker(pairsMap[p])
		if e != nil {
			return nil, fmt.Errorf("error while fetching ticker price for trading pair %s: %s", pairsMap[p], e)
		}

		askPrice, e := utils.CheckFetchFloat(tickerMap, "ask")
		if e != nil {
			return nil, fmt.Errorf("unable to correctly fetch value from tickerMap: %s", e)
		}
		bidPrice, e := utils.CheckFetchFloat(tickerMap, "bid")
		if e != nil {
			return nil, fmt.Errorf("unable to correctly fetch value from tickerMap: %s", e)
		}

		priceResult[p] = api.Ticker{
			AskPrice: model.NumberFromFloat(askPrice, c.precision),
			BidPrice: model.NumberFromFloat(bidPrice, c.precision),
		}
	}

	return priceResult, nil
}

// GetAssetConverter impl
func (c ccxtExchange) GetAssetConverter() *model.AssetConverter {
	return c.assetConverter
}

// GetOrderConstraints impl
func (c ccxtExchange) GetOrderConstraints(pair *model.TradingPair) *model.OrderConstraints {
	// TODO implement
	return nil
}

// GetAccountBalances impl
func (c ccxtExchange) GetAccountBalances(assetList []model.Asset) (map[model.Asset]model.Number, error) {
	balanceResponse, e := c.api.FetchBalance()
	if e != nil {
		return nil, e
	}

	m := map[model.Asset]model.Number{}
	for _, asset := range assetList {
		ccxtAssetString, e := c.GetAssetConverter().ToString(asset)
		if e != nil {
			return nil, e
		}

		if ccxtBalance, ok := balanceResponse[ccxtAssetString]; ok {
			m[asset] = *model.NumberFromFloat(ccxtBalance.Total, c.precision)
		} else {
			m[asset] = *model.NumberFromFloat(0, c.precision)
		}
	}
	return m, nil
}

// GetOrderBook impl
func (c ccxtExchange) GetOrderBook(pair *model.TradingPair, maxCount int32) (*model.OrderBook, error) {
	pairString, e := pair.ToString(c.assetConverter, c.delimiter)
	if e != nil {
		return nil, fmt.Errorf("error converting pair to string: %s", e)
	}

	limit := int(maxCount)
	ob, e := c.api.FetchOrderBook(pairString, &limit)
	if e != nil {
		return nil, fmt.Errorf("error while fetching orderbook for trading pair '%s': %s", pairString, e)
	}

	if _, ok := ob["asks"]; !ok {
		return nil, fmt.Errorf("orderbook did not contain the 'asks' field: %v", ob)
	}
	if _, ok := ob["bids"]; !ok {
		return nil, fmt.Errorf("orderbook did not contain the 'bids' field: %v", ob)
	}

	asks := c.readOrders(ob["asks"], pair, model.OrderActionSell)
	bids := c.readOrders(ob["bids"], pair, model.OrderActionBuy)
	return model.MakeOrderBook(pair, asks, bids), nil
}

func (c ccxtExchange) readOrders(orders []sdk.CcxtOrder, pair *model.TradingPair, orderAction model.OrderAction) []model.Order {
	result := []model.Order{}
	for _, o := range orders {
		result = append(result, model.Order{
			Pair:        pair,
			OrderAction: orderAction,
			OrderType:   model.OrderTypeLimit,
			Price:       model.NumberFromFloat(o.Price, c.precision),
			Volume:      model.NumberFromFloat(o.Amount, c.precision),
			Timestamp:   nil,
		})
	}
	return result
}

// GetTrades impl
func (c ccxtExchange) GetTrades(pair *model.TradingPair, maybeCursor interface{}) (*api.TradesResult, error) {
	pairString, e := pair.ToString(c.assetConverter, c.delimiter)
	if e != nil {
		return nil, fmt.Errorf("error converting pair to string: %s", e)
	}

	// TODO use cursor when fetching trades
	tradesRaw, e := c.api.FetchTrades(pairString)
	if e != nil {
		return nil, fmt.Errorf("error while fetching trades for trading pair '%s': %s", pairString, e)
	}

	trades := []model.Trade{}
	for _, raw := range tradesRaw {
		t, e := c.readTrade(pair, pairString, raw)
		if e != nil {
			return nil, fmt.Errorf("error while reading trade: %s", e)
		}
		trades = append(trades, *t)
	}

	// TODO implement cursor logic
	return &api.TradesResult{
		Cursor: nil,
		Trades: trades,
	}, nil
}

func (c ccxtExchange) readTrade(pair *model.TradingPair, pairString string, rawTrade sdk.CcxtTrade) (*model.Trade, error) {
	if rawTrade.Symbol != pairString {
		return nil, fmt.Errorf("expected '%s' for 'symbol' field, got: %s", pairString, rawTrade.Symbol)
	}

	trade := model.Trade{
		Order: model.Order{
			Pair:      pair,
			Price:     model.NumberFromFloat(rawTrade.Price, c.precision),
			Volume:    model.NumberFromFloat(rawTrade.Amount, c.precision),
			OrderType: model.OrderTypeLimit,
			Timestamp: model.MakeTimestamp(rawTrade.Timestamp),
		},
		TransactionID: model.MakeTransactionID(rawTrade.ID),
		Fee:           nil,
	}

	if rawTrade.Side == "sell" {
		trade.OrderAction = model.OrderActionSell
	} else if rawTrade.Side == "buy" {
		trade.OrderAction = model.OrderActionBuy
	} else {
		return nil, fmt.Errorf("unrecognized value for 'side' field: %s", rawTrade.Side)
	}

	if rawTrade.Cost != 0.0 {
		// use 2x the precision for cost since it's logically derived from amount and price
		trade.Cost = model.NumberFromFloat(rawTrade.Cost, c.precision*2)
	}

	return &trade, nil
}

// GetTradeHistory impl
func (c ccxtExchange) GetTradeHistory(maybeCursorStart interface{}, maybeCursorEnd interface{}) (*api.TradeHistoryResult, error) {
	// TODO implement
	return nil, nil
}

// GetOpenOrders impl
func (c ccxtExchange) GetOpenOrders(pairs []*model.TradingPair) (map[model.TradingPair][]model.OpenOrder, error) {
	pairStrings := []string{}
	string2Pair := map[string]model.TradingPair{}
	for _, pair := range pairs {
		pairString, e := pair.ToString(c.assetConverter, c.delimiter)
		if e != nil {
			return nil, fmt.Errorf("error converting pairs to strings: %s", e)
		}
		pairStrings = append(pairStrings, pairString)
		string2Pair[pairString] = *pair
	}

	openOrdersMap, e := c.api.FetchOpenOrders(pairStrings)
	if e != nil {
		return nil, fmt.Errorf("error while fetching open orders for trading pairs '%v': %s", pairStrings, e)
	}

	result := map[model.TradingPair][]model.OpenOrder{}
	for asset, ccxtOrderList := range openOrdersMap {
		pair, ok := string2Pair[asset]
		if !ok {
			return nil, fmt.Errorf("traing symbol %s returned from FetchOpenOrders was not in the original list of trading pairs: %v", asset, pairStrings)
		}

		openOrderList := []model.OpenOrder{}
		for _, o := range ccxtOrderList {
			openOrder, e := c.convertOpenOrderFromCcxt(&pair, o)
			if e != nil {
				return nil, fmt.Errorf("cannot convertOpenOrderFromCcxt: %s", e)
			}
			openOrderList = append(openOrderList, *openOrder)
		}
		result[pair] = openOrderList
	}
	return result, nil
}

func (c ccxtExchange) convertOpenOrderFromCcxt(pair *model.TradingPair, o sdk.CcxtOpenOrder) (*model.OpenOrder, error) {
	if o.Type != "limit" {
		return nil, fmt.Errorf("we currently only support limit order types")
	}

	orderAction := model.OrderActionSell
	if o.Side == "buy" {
		orderAction = model.OrderActionBuy
	}
	ts := model.MakeTimestamp(o.Timestamp)

	return &model.OpenOrder{
		Order: model.Order{
			Pair:        pair,
			OrderAction: orderAction,
			OrderType:   model.OrderTypeLimit,
			Price:       model.NumberFromFloat(o.Price, c.precision),
			Volume:      model.NumberFromFloat(o.Amount, c.precision),
			Timestamp:   ts,
		},
		ID:             o.ID,
		StartTime:      ts,
		ExpireTime:     nil,
		VolumeExecuted: model.NumberFromFloat(o.Filled, c.precision),
	}, nil
}

// AddOrder impl
func (c ccxtExchange) AddOrder(order *model.Order) (*model.TransactionID, error) {
	pairString, e := order.Pair.ToString(c.assetConverter, c.delimiter)
	if e != nil {
		return nil, fmt.Errorf("error converting pair to string: %s", e)
	}

	side := "sell"
	if order.OrderAction.IsBuy() {
		side = "buy"
	}
	ccxtOpenOrder, e := c.api.CreateLimitOrder(pairString, side, order.Volume.AsFloat(), order.Price.AsFloat())
	if e != nil {
		return nil, fmt.Errorf("error while creating limit order %s: %s", *order, e)
	}

	return model.MakeTransactionID(ccxtOpenOrder.ID), nil
}

// CancelOrder impl
func (c ccxtExchange) CancelOrder(txID *model.TransactionID) (model.CancelOrderResult, error) {
	// TODO implement
	return model.CancelResultCancelSuccessful, nil
}

// PrepareDeposit impl
func (c ccxtExchange) PrepareDeposit(asset model.Asset, amount *model.Number) (*api.PrepareDepositResult, error) {
	// TODO implement
	return nil, nil
}

// GetWithdrawInfo impl
func (c ccxtExchange) GetWithdrawInfo(asset model.Asset, amountToWithdraw *model.Number, address string) (*api.WithdrawInfo, error) {
	// TODO implement
	return nil, nil
}

// WithdrawFunds impl
func (c ccxtExchange) WithdrawFunds(
	asset model.Asset,
	amountToWithdraw *model.Number,
	address string,
) (*api.WithdrawFunds, error) {
	// TODO implement
	return nil, nil
}
