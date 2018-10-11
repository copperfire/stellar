package plugins

import (
	"fmt"
	"log"

	"github.com/lightyeario/kelp/api"
	"github.com/lightyeario/kelp/support/utils"
	"github.com/stellar/go/clients/horizon"
	"github.com/stellar/go/support/config"
)

// StrategyContainer contains the strategy factory method along with some metadata
type StrategyContainer struct {
	SortOrder   uint8
	Description string
	NeedsConfig bool
	Complexity  string
	makeFn      func(sdex *SDEX, assetBase *horizon.Asset, assetQuote *horizon.Asset, stratConfigPath string) (api.Strategy, error)
}

// strategies is a map of all the strategies available
var strategies = map[string]StrategyContainer{
	"buysell": StrategyContainer{
		SortOrder:   1,
		Description: "Creates buy and sell offers based on a reference price with a pre-specified liquidity depth",
		NeedsConfig: true,
		Complexity:  "Beginner",
		makeFn: func(sdex *SDEX, assetBase *horizon.Asset, assetQuote *horizon.Asset, stratConfigPath string) (api.Strategy, error) {
			var cfg buySellConfig
			err := config.Read(stratConfigPath, &cfg)
			utils.CheckConfigError(cfg, err, stratConfigPath)
			return makeBuySellStrategy(sdex, assetBase, assetQuote, &cfg)
		},
	},
	"mirror": StrategyContainer{
		SortOrder:   4,
		Description: "Mirrors an orderbook from another exchange by placing the same orders on Stellar",
		NeedsConfig: true,
		Complexity:  "Advanced",
		makeFn: func(sdex *SDEX, assetBase *horizon.Asset, assetQuote *horizon.Asset, stratConfigPath string) (api.Strategy, error) {
			var cfg mirrorConfig
			err := config.Read(stratConfigPath, &cfg)
			utils.CheckConfigError(cfg, err, stratConfigPath)
			return makeMirrorStrategy(sdex, assetBase, assetQuote, &cfg)
		},
	},
	"sell": StrategyContainer{
		SortOrder:   0,
		Description: "Creates sell offers based on a reference price with a pre-specified liquidity depth",
		NeedsConfig: true,
		Complexity:  "Beginner",
		makeFn: func(sdex *SDEX, assetBase *horizon.Asset, assetQuote *horizon.Asset, stratConfigPath string) (api.Strategy, error) {
			var cfg sellConfig
			err := config.Read(stratConfigPath, &cfg)
			utils.CheckConfigError(cfg, err, stratConfigPath)
			return makeSellStrategy(sdex, assetBase, assetQuote, &cfg)
		},
	},
	"balanced": StrategyContainer{
		SortOrder:   3,
		Description: "Dynamically prices two tokens based on their relative demand",
		NeedsConfig: true,
		Complexity:  "Intermediate",
		makeFn: func(sdex *SDEX, assetBase *horizon.Asset, assetQuote *horizon.Asset, stratConfigPath string) (api.Strategy, error) {
			var cfg balancedConfig
			err := config.Read(stratConfigPath, &cfg)
			utils.CheckConfigError(cfg, err, stratConfigPath)
			return makeBalancedStrategy(sdex, assetBase, assetQuote, &cfg), nil
		},
	},
	"delete": StrategyContainer{
		SortOrder:   2,
		Description: "Deletes all orders for the configured orderbook",
		NeedsConfig: false,
		Complexity:  "Beginner",
		makeFn: func(sdex *SDEX, assetBase *horizon.Asset, assetQuote *horizon.Asset, stratConfigPath string) (api.Strategy, error) {
			return makeDeleteStrategy(sdex, assetBase, assetQuote), nil
		},
	},
}

// MakeStrategy makes a strategy
func MakeStrategy(
	sdex *SDEX,
	assetBase *horizon.Asset,
	assetQuote *horizon.Asset,
	strategy string,
	stratConfigPath string,
) (api.Strategy, error) {
	if strat, ok := strategies[strategy]; ok {
		if strat.NeedsConfig && stratConfigPath == "" {
			log.Println()
			log.Fatalf("error: the '%s' strategy needs a config file\n", strategy)
		}
		return strat.makeFn(sdex, assetBase, assetQuote, stratConfigPath)
	}

	return nil, fmt.Errorf("invalid strategy type: %s", strategy)
}

// Strategies returns the list of strategies along with metadata
func Strategies() map[string]StrategyContainer {
	return strategies
}

type exchangeContainer struct {
	description string
	makeFn      func() (api.Exchange, error)
}

// exchanges is a map of all the exchange integrations available
var exchanges = map[string]exchangeContainer{
	"kraken": exchangeContainer{
		description: "Kraken is a popular centralized cryptocurrency exchange (https://www.kraken.com/)",
		makeFn:      makeKrakenExchange,
	},
	"ccxt-binance": exchangeContainer{
		description: "Binance is a popular centralized cryptocurrency exchange (via ccxt-rest)",
		makeFn: func() (api.Exchange, error) {
			// TODO this URL should be taken from the bot config and this factory.go needs to be structified
			return makeCcxtExchange("http://localhost:3000", "binance")
		},
	},
}

// MakeExchange is a factory method to make an exchange based on a given type
func MakeExchange(exchangeType string) (api.Exchange, error) {
	if exchange, ok := exchanges[exchangeType]; ok {
		return exchange.makeFn()
	}

	log.Fatalf("invalid exchange type: %s\n", exchangeType)
	return nil, nil
}

// Exchanges returns the list of exchanges along with the description
func Exchanges() map[string]string {
	m := make(map[string]string, len(exchanges))
	for name := range exchanges {
		m[name] = exchanges[name].description
	}
	return m
}
