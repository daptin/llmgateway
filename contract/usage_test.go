package contract

import "testing"

func TestUsageValidAcceptsIndependentMeasures(t *testing.T) {
	usage := Usage{InputTokens: 3, TotalTokens: 3, Measures: map[string]int64{
		"request_bytes":  24,
		"search-results": 2,
		"vendor.credits": 7,
	}}
	if !usage.Valid() {
		t.Fatalf("valid usage rejected: %#v", usage)
	}
}

func TestUsageValidRejectsInvalidOrConflictingMeasures(t *testing.T) {
	for name, measures := range map[string]map[string]int64{
		"negative":  {"request_bytes": -1},
		"uppercase": {"RequestBytes": 1},
		"space":     {"request bytes": 1},
		"reserved":  {"total_tokens": 1},
	} {
		t.Run(name, func(t *testing.T) {
			if (Usage{Measures: measures}).Valid() {
				t.Fatalf("invalid usage accepted: %#v", measures)
			}
		})
	}
}

func TestUsageAllMeasuresReturnsIndependentCanonicalProjection(t *testing.T) {
	usage := Usage{InputTokens: 3, TotalTokens: 3, Measures: map[string]int64{"request_bytes": 24, "total_tokens": 999}}
	measures := usage.AllMeasures()
	if measures["request_bytes"] != 24 || measures["input_tokens"] != 3 || measures["total_tokens"] != 3 {
		t.Fatalf("all measures = %#v", measures)
	}
	measures["request_bytes"] = 0
	if usage.Measures["request_bytes"] != 24 {
		t.Fatal("AllMeasures returned an alias of supplemental measures")
	}
}
