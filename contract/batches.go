package contract

import "time"

type BatchStatus string

const (
	BatchStatusValidating BatchStatus = "validating"
	BatchStatusFailed     BatchStatus = "failed"
	BatchStatusInProgress BatchStatus = "in_progress"
	BatchStatusFinalizing BatchStatus = "finalizing"
	BatchStatusCompleted  BatchStatus = "completed"
	BatchStatusExpired    BatchStatus = "expired"
	BatchStatusCancelling BatchStatus = "cancelling"
	BatchStatusCancelled  BatchStatus = "cancelled"
)

func (status BatchStatus) Valid() bool {
	switch status {
	case BatchStatusValidating, BatchStatusFailed, BatchStatusInProgress, BatchStatusFinalizing,
		BatchStatusCompleted, BatchStatusExpired, BatchStatusCancelling, BatchStatusCancelled:
		return true
	default:
		return false
	}
}

type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
	Line    *int64 `json:"line,omitempty"`
}

type BatchRequestCounts struct {
	Total     int64 `json:"total"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
}

type Batch struct {
	ID               ID
	Endpoint         string
	Errors           []BatchError
	InputFileID      ID
	CompletionWindow string
	Status           BatchStatus
	OutputFileID     ID
	ErrorFileID      ID
	CreatedAt        time.Time
	InProgressAt     *time.Time
	ExpiresAt        *time.Time
	FinalizingAt     *time.Time
	CompletedAt      *time.Time
	FailedAt         *time.Time
	ExpiredAt        *time.Time
	CancellingAt     *time.Time
	CancelledAt      *time.Time
	RequestCounts    BatchRequestCounts
	Metadata         map[string]string
}

type CreateBatchRequest struct {
	InputFileID        ID
	Endpoint           string
	CompletionWindow   string
	Metadata           map[string]string
	OutputExpiresAfter *FileExpiration
}

type ListBatchesRequest struct {
	After ID
	Limit int
}

type BatchPage struct {
	Data    []Batch
	HasMore bool
}
