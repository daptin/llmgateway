package contract

import "time"

// FilePurpose identifies the OpenAI-compatible workflow that owns a file.
type FilePurpose string

const (
	FilePurposeAssistants       FilePurpose = "assistants"
	FilePurposeAssistantsOutput FilePurpose = "assistants_output"
	FilePurposeBatch            FilePurpose = "batch"
	FilePurposeBatchOutput      FilePurpose = "batch_output"
	FilePurposeFineTune         FilePurpose = "fine-tune"
	FilePurposeFineTuneResults  FilePurpose = "fine-tune-results"
	FilePurposeVision           FilePurpose = "vision"
	FilePurposeUserData         FilePurpose = "user_data"
	FilePurposeEvals            FilePurpose = "evals"
	FilePurposeMessages         FilePurpose = "messages"
)

func (purpose FilePurpose) Valid() bool {
	switch purpose {
	case FilePurposeAssistants, FilePurposeAssistantsOutput, FilePurposeBatch, FilePurposeBatchOutput,
		FilePurposeFineTune, FilePurposeFineTuneResults, FilePurposeVision, FilePurposeUserData,
		FilePurposeEvals, FilePurposeMessages:
		return true
	default:
		return false
	}
}

type File struct {
	ID            ID
	Bytes         int64
	CreatedAt     time.Time
	Filename      string
	ContentType   string
	Purpose       FilePurpose
	Status        string
	StatusDetails string
	ExpiresAt     *time.Time
}

type FileExpiration struct {
	Anchor  string
	Seconds int64
}

type CreateFileRequest struct {
	Filename     string
	ContentType  string
	Purpose      FilePurpose
	Data         []byte
	ExpiresAfter *FileExpiration
}

type ListFilesRequest struct {
	Purpose FilePurpose
	After   ID
	Limit   int
	Order   string
}

type FilePage struct {
	Data    []File
	HasMore bool
}

type FileContent struct {
	Filename    string
	ContentType string
	Data        []byte
}
