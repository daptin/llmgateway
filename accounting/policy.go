package accounting

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type Binding struct {
	ScopeKind string
	ScopeID   contract.ID
	Policy    catalog.Policy
}

type Measures struct {
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	CostMicros   int64
	ComputeUnits int64
	Bytes        int64
	Concurrency  int64
}

func Reservations(bindings []Binding, measures Measures, now time.Time) ([]contract.LimitReservation, error) {
	if err := validateMeasures(measures); err != nil {
		return nil, err
	}
	result := make([]contract.LimitReservation, 0)
	seen := make(map[string]struct{})
	for _, binding := range bindings {
		if binding.ScopeKind == "" || binding.ScopeID == "" || binding.Policy.ID == "" {
			return nil, errors.New("policy binding scope and policy identifiers are required")
		}
		for _, limit := range binding.Policy.Limits {
			if limit.Mode == "soft" {
				continue
			}
			amount, err := metricValue(limit.Metric, measures)
			if err != nil {
				return nil, fmt.Errorf("policy %q: %w", binding.Policy.ID, err)
			}
			start, end, err := windowBounds(limit.Metric, limit.Window, now)
			if err != nil {
				return nil, fmt.Errorf("policy %q metric %q: %w", binding.Policy.ID, limit.Metric, err)
			}
			key := strings.Join([]string{binding.ScopeKind, string(binding.ScopeID), string(binding.Policy.ID), limit.Metric, start.UTC().Format(time.RFC3339Nano)}, "\x00")
			if _, duplicate := seen[key]; duplicate {
				return nil, fmt.Errorf("duplicate effective limit for %s/%s/%s/%s", binding.ScopeKind, binding.ScopeID, binding.Policy.ID, limit.Metric)
			}
			seen[key] = struct{}{}
			result = append(result, contract.LimitReservation{
				ScopeKind: binding.ScopeKind, ScopeID: binding.ScopeID, PolicyID: binding.Policy.ID,
				Metric: limit.Metric, Window: limit.Window, WindowStart: start, WindowEnd: end,
				Maximum: limit.Maximum, Amount: amount,
			})
		}
	}
	return result, nil
}

func validateMeasures(measures Measures) error {
	values := []int64{measures.Requests, measures.InputTokens, measures.OutputTokens, measures.TotalTokens,
		measures.CostMicros, measures.ComputeUnits, measures.Bytes, measures.Concurrency}
	for _, value := range values {
		if value < 0 {
			return errors.New("policy measures cannot be negative")
		}
	}
	return nil
}

func metricValue(metric string, measures Measures) (int64, error) {
	switch metric {
	case "requests":
		return measures.Requests, nil
	case "input_tokens":
		return measures.InputTokens, nil
	case "output_tokens":
		return measures.OutputTokens, nil
	case "total_tokens":
		return measures.TotalTokens, nil
	case "cost_micros":
		return measures.CostMicros, nil
	case "compute_units":
		return measures.ComputeUnits, nil
	case "bytes":
		return measures.Bytes, nil
	case "concurrency":
		return measures.Concurrency, nil
	default:
		return 0, fmt.Errorf("unknown policy metric %q", metric)
	}
}

func windowBounds(metric, window string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	if metric == "concurrency" {
		if window != "" {
			return time.Time{}, time.Time{}, errors.New("concurrency limit cannot declare a window")
		}
		return time.Time{}, time.Time{}, nil
	}
	if window == "calendar_month" {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), nil
	}
	duration, err := parseFixedWindow(window)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start := now.Truncate(duration)
	return start, start.Add(duration), nil
}

func parseFixedWindow(value string) (time.Duration, error) {
	if len(value) < 2 {
		return 0, fmt.Errorf("invalid fixed window %q", value)
	}
	count, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("invalid fixed window %q", value)
	}
	var unit time.Duration
	switch value[len(value)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid fixed window %q", value)
	}
	if count > int64((1<<63-1)/unit) {
		return 0, fmt.Errorf("fixed window %q overflows duration", value)
	}
	return time.Duration(count) * unit, nil
}
