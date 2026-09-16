package wire

// Video generation. Unlike chat, embeddings, and image generation
// — all synchronous — every video provider is asynchronous: the gateway submits
// a job and the client polls until it reaches a terminal status. The provider
// seam is provider.VideoGenerator (SubmitVideo + PollVideo); the HTTP surface is
// POST /v1/videos/generations (202 + operation id) and GET /v1/videos/{id}.

// VideoStatus is the lifecycle state of an asynchronous video-generation
// operation.
type VideoStatus string

const (
	VideoStatusQueued    VideoStatus = "queued"
	VideoStatusRunning   VideoStatus = "running"
	VideoStatusSucceeded VideoStatus = "succeeded"
	VideoStatusFailed    VideoStatus = "failed"
)

// VideoOperationObject is the constant "object" discriminator on VideoOperation,
// mirroring OpenAI's typed-object convention.
const VideoOperationObject = "video.operation"

// GenerateVideoRequest is the gateway request for text-to-video generation
// (POST /v1/videos/generations). Providers normalize their native parameters
// behind the provider.VideoGenerator seam; a field a given provider doesn't
// support is ignored rather than rejected, matching how image generation
// degrades capability gaps. (Image-to-video — a seed image — is a follow-up.)
type GenerateVideoRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	NegativePrompt  string `json:"negative_prompt,omitempty"`
	AspectRatio     string `json:"aspect_ratio,omitempty"`     // e.g. "16:9", "9:16"
	Resolution      string `json:"resolution,omitempty"`       // e.g. "720p", "1080p"
	DurationSeconds int    `json:"duration_seconds,omitempty"` // requested clip length
	N               int    `json:"n,omitempty"`                // number of videos (provider may cap)
	User            string `json:"user,omitempty"`
}

// VideoOperation is the gateway's normalized handle to an async video job. ID is
// a gateway-issued opaque token; clients poll GET /v1/videos/{id} with it until
// Status is terminal. The gateway holds no operation state — the token encodes
// everything needed to resume the poll against the owning provider, which holds
// the job — so a poll survives a gateway restart without an operation store.
type VideoOperation struct {
	ID     string        `json:"id"`
	Object string        `json:"object"` // always VideoOperationObject
	Model  string        `json:"model,omitempty"`
	Status VideoStatus   `json:"status"`
	Videos []VideoResult `json:"videos,omitempty"` // populated once Status == succeeded
	Error  string        `json:"error,omitempty"`  // populated once Status == failed
}

// VideoResult is one generated video. URL points at the playable asset. On the
// GET /v1/videos/{id} surface it is a gateway content URL
// (/v1/videos/{id}/content?index=N) the client fetches with its own bearer; the
// gateway proxies the bytes from the upstream with the provider credential.
// Persisting the bytes to durable gateway-hosted storage — rather than
// re-fetching the (expiring) upstream asset on each request — remains a
// follow-up.
type VideoResult struct {
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}
