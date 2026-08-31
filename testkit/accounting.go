package testkit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daptin/llmgateway/accounting"
	"github.com/daptin/llmgateway/contract"
)

type AccountingStore struct {
	mu       sync.Mutex
	next     uint64
	requests map[contract.ID]*accountingRequest
	buckets  map[string]*accountingBucket
}

type accountingRequest struct {
	token        contract.ReservationToken
	state        string
	reservations []contract.LimitReservation
	completion   *contract.Completion
	cancellation *contract.Cancellation
	expiresAt    time.Time
}

type accountingBucket struct {
	consumed int64
	reserved int64
}

func NewAccountingStore() *AccountingStore {
	return &AccountingStore{requests: make(map[contract.ID]*accountingRequest), buckets: make(map[string]*accountingBucket)}
}

func (s *AccountingStore) Admit(ctx context.Context, admission contract.Admission) (contract.ReservationToken, error) {
	if err := ctx.Err(); err != nil {
		return contract.ReservationToken{}, err
	}
	if admission.RequestID == "" {
		return contract.ReservationToken{}, fmt.Errorf("%w: empty request id", accounting.ErrDuplicateRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.requests[admission.RequestID]; exists {
		return contract.ReservationToken{}, accounting.ErrDuplicateRequest
	}
	for _, reservation := range admission.LimitReservations {
		bucket := s.buckets[bucketKey(reservation)]
		if bucket != nil && bucket.consumed+bucket.reserved+reservation.Amount > reservation.Maximum {
			return contract.ReservationToken{}, accounting.ErrLimitExceeded
		}
		if bucket == nil && reservation.Amount > reservation.Maximum {
			return contract.ReservationToken{}, accounting.ErrLimitExceeded
		}
	}
	s.next++
	token := contract.ReservationToken{RequestID: admission.RequestID, Opaque: fmt.Sprintf("reservation-%d", s.next)}
	for _, reservation := range admission.LimitReservations {
		key := bucketKey(reservation)
		bucket := s.buckets[key]
		if bucket == nil {
			bucket = &accountingBucket{}
			s.buckets[key] = bucket
		}
		bucket.reserved += reservation.Amount
	}
	s.requests[admission.RequestID] = &accountingRequest{
		token: token, state: "held", reservations: append([]contract.LimitReservation(nil), admission.LimitReservations...),
		expiresAt: admission.StartedAt.Add(time.Minute),
	}
	return token, nil
}

func (s *AccountingStore) Finalize(ctx context.Context, completion contract.Completion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	request, err := s.held(completion.Token)
	if err != nil {
		if request != nil && request.state == "finalized" {
			return nil
		}
		return err
	}
	for _, reservation := range request.reservations {
		bucket := s.buckets[bucketKey(reservation)]
		bucket.reserved -= reservation.Amount
		if reservation.Metric != "concurrency" {
			bucket.consumed += actualAmount(reservation, completion.Usage)
		}
	}
	copy := completion
	copy.Attempts = append([]contract.Attempt(nil), completion.Attempts...)
	request.completion = &copy
	request.state = "finalized"
	return nil
}

func (s *AccountingStore) Cancel(ctx context.Context, cancellation contract.Cancellation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	request, err := s.held(cancellation.Token)
	if err != nil {
		if request != nil && request.state == "cancelled" {
			return nil
		}
		return err
	}
	for _, reservation := range request.reservations {
		bucket := s.buckets[bucketKey(reservation)]
		bucket.reserved -= reservation.Amount
		if reservation.Metric != "concurrency" {
			bucket.consumed += actualAmount(reservation, cancellation.Usage)
		}
	}
	copy := cancellation
	copy.Attempts = append([]contract.Attempt(nil), cancellation.Attempts...)
	request.cancellation = &copy
	request.state = "cancelled"
	return nil
}

func (s *AccountingStore) ReapExpired(ctx context.Context, now time.Time, limit int) (contract.ReapResult, error) {
	if err := ctx.Err(); err != nil {
		return contract.ReapResult{}, err
	}
	if limit <= 0 {
		return contract.ReapResult{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := contract.ReapResult{}
	for _, request := range s.requests {
		if result.Examined >= limit {
			break
		}
		if request.state != "held" || request.expiresAt.After(now) {
			continue
		}
		result.Examined++
		for _, reservation := range request.reservations {
			s.buckets[bucketKey(reservation)].reserved -= reservation.Amount
		}
		request.state = "cancelled"
		request.cancellation = &contract.Cancellation{Token: request.token, Reason: "lease_expired", EndedAt: now}
		result.Released++
	}
	return result, nil
}

func (s *AccountingStore) State(requestID contract.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request := s.requests[requestID]; request != nil {
		return request.state
	}
	return ""
}

func (s *AccountingStore) Completion(requestID contract.ID) (contract.Completion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request := s.requests[requestID]
	if request == nil || request.completion == nil {
		return contract.Completion{}, false
	}
	completion := *request.completion
	completion.Attempts = append([]contract.Attempt(nil), request.completion.Attempts...)
	return completion, true
}

func (s *AccountingStore) held(token contract.ReservationToken) (*accountingRequest, error) {
	request := s.requests[token.RequestID]
	if request == nil || request.token.Opaque != token.Opaque {
		return nil, accounting.ErrUnknownRequest
	}
	if request.state != "held" {
		return request, accounting.ErrInvalidTransition
	}
	return request, nil
}

func bucketKey(reservation contract.LimitReservation) string {
	return strings.Join([]string{reservation.ScopeKind, string(reservation.ScopeID), string(reservation.PolicyID), reservation.Metric,
		reservation.WindowStart.UTC().Format(time.RFC3339Nano)}, "\x00")
}

func actualAmount(reservation contract.LimitReservation, usage contract.Usage) int64 {
	switch reservation.Metric {
	case "input_tokens":
		return usage.InputTokens
	case "output_tokens":
		return usage.OutputTokens
	case "total_tokens":
		return usage.TotalTokens
	case "cost_micros":
		return usage.CostMicros
	default:
		return reservation.Amount
	}
}
