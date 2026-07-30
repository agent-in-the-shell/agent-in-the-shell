package agentmodel

// Wire-contract aliases.
//
// ChatRequest and friends moved to services/agentmodel/wire so that services
// which merely CALL the gateway over HTTP can compile against the contract
// without pulling in the server that implements it. Before the split, importing
// this package dragged in 23 packages — the router, every provider, the store —
// because `go list -deps -test` follows this package's own test imports, and
// cmd/release-export resolves publishable closures the same way.
//
// These aliases keep the ~90 server-side files under services/agentmodel/**
// compiling unchanged. They are scaffolding for the server side, NOT an
// invitation for new consumers: anything outside this service should import
// services/agentmodel/wire (types) and services/agentmodel/gateway (client)
// directly. internal/releasemanifest's boundary tests enforce that.
//
// Note: type aliases do not carry unexported fields across a package boundary —
// any code touching Message.rawContent must live in the wire package itself.

import "github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"

type (
	ChatRequest          = wire.ChatRequest
	ChatResponse         = wire.ChatResponse
	Choice               = wire.Choice
	Embedding            = wire.Embedding
	EmbeddingRequest     = wire.EmbeddingRequest
	EmbeddingResponse    = wire.EmbeddingResponse
	Error                = wire.Error
	FunctionSchema       = wire.FunctionSchema
	GenerateVideoRequest = wire.GenerateVideoRequest
	ImageData            = wire.ImageData
	ImageRequest         = wire.ImageRequest
	ImageResponse        = wire.ImageResponse
	Message              = wire.Message
	ThinkingBlock        = wire.ThinkingBlock
	ThinkingConfig       = wire.ThinkingConfig
	Tool                 = wire.Tool
	ToolCall             = wire.ToolCall
	ToolCallFunction     = wire.ToolCallFunction
	Usage                = wire.Usage
	VideoOperation       = wire.VideoOperation
	VideoResult          = wire.VideoResult
	VideoStatus          = wire.VideoStatus
)

const (
	AuthModeAPIKey            = wire.AuthModeAPIKey
	AuthModeSubscription      = wire.AuthModeSubscription
	CodeAuthUnavailable       = wire.CodeAuthUnavailable
	CodeBudgetExceeded        = wire.CodeBudgetExceeded
	CodeBudgetUnavailable     = wire.CodeBudgetUnavailable
	CodeContentPolicy         = wire.CodeContentPolicy
	CodeContextLength         = wire.CodeContextLength
	CodeEmptyArray            = wire.CodeEmptyArray
	CodeInvalidAPIKey         = wire.CodeInvalidAPIKey
	CodeInvalidJSON           = wire.CodeInvalidJSON
	CodeMasterRequired        = wire.CodeMasterRequired
	CodeMissingRequiredParam  = wire.CodeMissingRequiredParam
	CodeModelAccessDenied     = wire.CodeModelAccessDenied
	CodeModelNotFound         = wire.CodeModelNotFound
	CodeQuotaExceeded         = wire.CodeQuotaExceeded
	CodeRateLimitExceeded     = wire.CodeRateLimitExceeded
	CodeRequestLogin          = wire.CodeRequestLogin
	ErrTypeAllDeploymentsFail = wire.ErrTypeAllDeploymentsFail
	ErrTypeAuthentication     = wire.ErrTypeAuthentication
	ErrTypeBudgetExceeded     = wire.ErrTypeBudgetExceeded
	ErrTypeContentFilter      = wire.ErrTypeContentFilter
	ErrTypeContextWindow      = wire.ErrTypeContextWindow
	ErrTypeInvalidRequest     = wire.ErrTypeInvalidRequest
	ErrTypeLoginRequired      = wire.ErrTypeLoginRequired
	ErrTypeNotFound           = wire.ErrTypeNotFound
	ErrTypePermissionDenied   = wire.ErrTypePermissionDenied
	ErrTypeRateLimit          = wire.ErrTypeRateLimit
	ErrTypeServiceUnavailable = wire.ErrTypeServiceUnavailable
	ErrTypeTimeout            = wire.ErrTypeTimeout
	ErrTypeUpstream           = wire.ErrTypeUpstream
	MaxRetryAfter             = wire.MaxRetryAfter
	VideoOperationObject      = wire.VideoOperationObject
	VideoStatusFailed         = wire.VideoStatusFailed
	VideoStatusQueued         = wire.VideoStatusQueued
	VideoStatusRunning        = wire.VideoStatusRunning
	VideoStatusSucceeded      = wire.VideoStatusSucceeded
)

var (
	ClampRetryAfter         = wire.ClampRetryAfter
	NewError                = wire.NewError
	NewErrorCode            = wire.NewErrorCode
	NewErrorf               = wire.NewErrorf
	ParseRetryAfter         = wire.ParseRetryAfter
	RetryAfterFor           = wire.RetryAfterFor
	RetryAfterFromHeader    = wire.RetryAfterFromHeader
	ValidateTextOnlyContent = wire.ValidateTextOnlyContent
	WithRetryAfter          = wire.WithRetryAfter
	Wrap                    = wire.Wrap
)
