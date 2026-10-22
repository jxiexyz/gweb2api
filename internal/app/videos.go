package app

// Video generation (Veo, async) was previously hardcoded to a separate Veo API endpoint,
// which failed on Google One AI Premium (Plus) accounts with "upstream returned no content frame".
// Video requests now route through the unified StreamGenerate path (Omni Flash)
// using custom payload flags ([17]=[[0]], [30]=[4,16,19], [41]=[1], [79]=1, [80]=1, [91]=1, [98]=1)
// and Google upstream extension headers (x-goog-ext-*).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

type VideoJob struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	Status    string `json:"status"` // queued | in_progress | completed | failed
	CreatedAt int64  `json:"created_at"`
	Prompt    string `json:"prompt,omitempty"`
	Response  string `json:"response,omitempty"`
	Error     string `json:"error,omitempty"`

	ConvID    string `json:"conversation_id,omitempty"`
}

var (
	videoJobs   = map[string]*VideoJob{}
	videoJobsMu sync.Mutex
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func PutVideoJob(j *VideoJob) {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	cutoff := time.Now().Unix() - 2*3600
	for id, old := range videoJobs {
		if old.CreatedAt < cutoff {
			delete(videoJobs, id)
		}
	}
	videoJobs[j.ID] = j
}

func GetVideoJob(id string) *VideoJob {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	return videoJobs[id]
}

// HandleCreateVideo handles POST /v1/videos (OpenAI Sora / video creation format).
func HandleCreateVideo(gem *gemini.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json body", http.StatusBadRequest)
		return
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		http.Error(w, "missing prompt", http.StatusBadRequest)
		return
	}
	model, _ := req["model"].(string)
	if model == "" {
		model = "gemini-video"
	}
	if model != "gemini-video" && model != "video" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "model must be a video model (gemini-video)",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	j := &VideoJob{
		ID:        "video_" + randHex(16),
		Object:    "video",
		Model:     model,
		Status:    "queued",
		CreatedAt: time.Now().Unix(),
		Prompt:    prompt,
	}
	PutVideoJob(j)

	go func() {
		videoJobsMu.Lock()
		j.Status = "in_progress"
		videoJobsMu.Unlock()

		resolved, err := models.Resolve(j.Model, "gemini-video")
		if err != nil {
			videoJobsMu.Lock()
			j.Status = "failed"
			j.Error = err.Error()
			videoJobsMu.Unlock()
			return
		}

		opt := gemini.ModelOptions{
			ModelName:   resolved.Name,
			ModelID:     resolved.Mode,
			ThinkMode:   resolved.Think,
			ModelHeader: resolved.Header,
			Extra:       resolved.Extra,
			IsVideo:     true,
		}

		// Route video requests through the same StreamGenerate path as text
		genResult, err := gem.Generate(j.Prompt, opt, nil)
		videoJobsMu.Lock()
		defer videoJobsMu.Unlock()
		if err != nil {
			j.Status = "failed"
			j.Error = err.Error()
			return
		}
		j.Status = "completed"
		j.Response = genResult.Text
		j.ConvID = genResult.ConversationID
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(j)
}

// HandleVideoItem handles GET /v1/videos/{id} and GET /v1/videos/{id}/content.
func HandleVideoItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
	parts := strings.Split(path, "/")
	id := parts[0]

	j := GetVideoJob(id)
	if j == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "video job not found",
				"type":    "not_found",
			},
		})
		return
	}

	if len(parts) > 1 && parts[1] == "content" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "video binary content not available via API; use conversation_id to download from Gemini Web UI",
				"type":    "not_implemented",
			},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(j)
}
