package wire

// ImageRequest is the OpenAI-compatible image-generation request
// (POST /v1/images/generations). Providers normalize their native shapes to
// this type behind the provider.ImageGenerator seam.
//
// Not every field maps cleanly onto every provider: Size/Quality are honored by
// OpenAI but best-effort on Gemini (its image models take no explicit pixel-size
// knob), and ResponseFormat="url" is only available where the upstream hosts the
// bytes — providers that return inline base64 (Gemini, gpt-image-1) always
// answer with b64_json regardless. Unsupported fields are ignored, never an
// error, matching how the gateway already degrades capability gaps elsewhere.
type ImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`               // number of images to generate (default 1)
	Size           string `json:"size,omitempty"`            // e.g. "1024x1024"
	Quality        string `json:"quality,omitempty"`         // e.g. "standard", "hd", "low", "medium", "high"
	ResponseFormat string `json:"response_format,omitempty"` // "b64_json" | "url"
	User           string `json:"user,omitempty"`
}

// ImageResponse is the OpenAI-compatible image-generation response.
//
// Usage is best-effort: gpt-image-1 reports input/output token counts, Gemini
// reports usageMetadata, and DALL·E reports nothing (zero usage). The gateway
// prices it through the same per-token cost path as chat/embeddings; models
// absent from the price registry resolve to $0 with cost_source="unpriced".
type ImageResponse struct {
	Created int64       `json:"created"`
	Model   string      `json:"model,omitempty"`
	Data    []ImageData `json:"data"`
	Usage   Usage       `json:"usage"`
}

// ImageData is one generated image. Exactly one of B64JSON / URL is populated,
// per the request's response_format (b64_json is the default for providers that
// only return inline bytes). RevisedPrompt is set when the upstream rewrote the
// prompt (e.g. OpenAI safety/quality rewrites).
type ImageData struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}
