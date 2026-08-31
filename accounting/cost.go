package accounting

import (
	"errors"
	"math/big"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

var ErrCostOverflow = errors.New("calculated cost exceeds int64")

const tokenRateDenominator = int64(1_000_000)

// CostMicros calculates fixed-point cost and rounds each non-zero component up
// to the nearest micro-unit. Rounding up is conservative for admission and is
// stable across architectures and database dialects.
func CostMicros(usage contract.Usage, pricing catalog.Pricing) (int64, error) {
	if !usage.Valid() {
		return 0, errors.New("usage values cannot be negative")
	}
	components := [][2]int64{
		{usage.InputTokens, pricing.InputMicrosPerMillion},
		{usage.OutputTokens, pricing.OutputMicrosPerMillion},
		{usage.CacheReadTokens, pricing.CacheReadMicrosPerMillion},
		{usage.CacheWriteTokens, pricing.CacheWriteMicrosPerMillion},
		{usage.ReasoningTokens, pricing.ReasoningMicrosPerMillion},
	}
	total := big.NewInt(0)
	for _, component := range components {
		if component[0] < 0 || component[1] < 0 {
			return 0, errors.New("usage and pricing values cannot be negative")
		}
		if component[0] == 0 || component[1] == 0 {
			continue
		}
		product := new(big.Int).Mul(big.NewInt(component[0]), big.NewInt(component[1]))
		product.Add(product, big.NewInt(tokenRateDenominator-1))
		product.Div(product, big.NewInt(tokenRateDenominator))
		total.Add(total, product)
	}
	if !total.IsInt64() {
		return 0, ErrCostOverflow
	}
	return total.Int64(), nil
}
