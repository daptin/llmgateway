package testkit

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/daptin/llmgateway/contract"
)

var (
	errDuplicateAdmission = errors.New("metering admission already exists")
	errUnknownAdmission   = errors.New("metering admission does not exist")
	errInvalidTransition  = errors.New("invalid metering lifecycle transition")
)

// MeteringRecorder is a host-port test double. It records lifecycle facts but
// deliberately does not interpret policies, quotas, or windows.
type MeteringRecorder struct {
	mu       sync.Mutex
	next     uint64
	requests map[contract.ID]*meteringRequest
	admitted []contract.Admission
	admitErr error
}

type meteringRequest struct {
	token        contract.ReservationToken
	state        string
	completion   *contract.Completion
	cancellation *contract.Cancellation
}

func NewMeteringRecorder() *MeteringRecorder {
	return &MeteringRecorder{requests: make(map[contract.ID]*meteringRequest)}
}

func (s *MeteringRecorder) Admit(ctx context.Context, admission contract.Admission) (contract.ReservationToken, error) {
	if err := ctx.Err(); err != nil {
		return contract.ReservationToken{}, err
	}
	if admission.RequestID == "" {
		return contract.ReservationToken{}, fmt.Errorf("%w: empty request id", errDuplicateAdmission)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.requests[admission.RequestID]; exists {
		return contract.ReservationToken{}, errDuplicateAdmission
	}
	s.admitted = append(s.admitted, admission)
	if s.admitErr != nil {
		return contract.ReservationToken{}, s.admitErr
	}
	s.next++
	token := contract.ReservationToken{RequestID: admission.RequestID, Opaque: fmt.Sprintf("reservation-%d", s.next)}
	s.requests[admission.RequestID] = &meteringRequest{
		token: token, state: "held",
	}
	return token, nil
}

func (s *MeteringRecorder) Complete(ctx context.Context, completion contract.Completion) error {
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
	copy := completion
	copy.Attempts = append([]contract.Attempt(nil), completion.Attempts...)
	request.completion = &copy
	request.state = "finalized"
	return nil
}

func (s *MeteringRecorder) Cancel(ctx context.Context, cancellation contract.Cancellation) error {
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
	copy := cancellation
	copy.Attempts = append([]contract.Attempt(nil), cancellation.Attempts...)
	request.cancellation = &copy
	request.state = "cancelled"
	return nil
}

func (s *MeteringRecorder) State(requestID contract.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request := s.requests[requestID]; request != nil {
		return request.state
	}
	return ""
}

func (s *MeteringRecorder) Completion(requestID contract.ID) (contract.Completion, bool) {
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

// RejectAdmissions configures the contract fake to fail admission without
// implementing a host policy language. Calls remain observable via Admissions.
func (s *MeteringRecorder) RejectAdmissions(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admitErr = err
}

func (s *MeteringRecorder) Admissions() []contract.Admission {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]contract.Admission, len(s.admitted))
	copy(result, s.admitted)
	return result
}

func (s *MeteringRecorder) held(token contract.ReservationToken) (*meteringRequest, error) {
	request := s.requests[token.RequestID]
	if request == nil || request.token.Opaque != token.Opaque {
		return nil, errUnknownAdmission
	}
	if request.state != "held" {
		return request, errInvalidTransition
	}
	return request, nil
}
