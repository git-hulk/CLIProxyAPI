// Package usagelog emits a JSON log line for each runtime's token usage record.
package usagelog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	pluginName             = "usage-jsonlog"
	consumerUsernameHeader = "X-Consumer-Username"
	fixedProjectID         = "codex-proxy"
	fixedProvider          = "OpenAI"
	usageLogBaseDir        = ".cli-proxy-api"
	usageLogSubdir         = "usage"
	usageLogDateFormat     = "2006-01-02"
)

func init() {
	coreusage.RegisterNamedPlugin(pluginName, &plugin{})
}

type plugin struct {
	mu       sync.Mutex
	file     *os.File
	fileDate string
}

// HandleUsage marshals the usage record into a compact JSON object and appends
// it as a single valid JSON line to ~/.cli-proxy-api/usage/{YYYY-MM-DD}.log.
// Records with no token usage and no failure are skipped to keep logs focused
// on real upstream consumption.
func (p *plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	if !hasReportableUsage(record) {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	consumer := consumerUsername(ctx)
	model := strings.TrimSpace(record.Model)

	usageDetailJSON := ""
	detail := usageDetail{
		CompletionTokens: record.Detail.OutputTokens,
		CompletionTokensDetails: completionTokensDetails{
			ReasoningTokens: record.Detail.ReasoningTokens,
		},
		LatencyCheckpoint: latencyCheckpoint{
			TotalDurationMs:   record.Latency.Milliseconds(),
			UserVisibleTTFTMs: record.TTFT.Milliseconds(),
		},
		PromptTokens: record.Detail.InputTokens,
		PromptTokensDetails: promptTokensDetails{
			CachedTokens: record.Detail.CachedTokens,
		},
		TotalTokens: record.Detail.TotalTokens,
	}
	if detailBytes, errMarshalDetail := json.Marshal(detail); errMarshalDetail == nil {
		usageDetailJSON = string(detailBytes)
	} else {
		log.WithError(errMarshalDetail).Debug("usagelog: failed to marshal usage detail")
	}

	payload := tokenUsageLog{
		Timestamp:        timestamp.UTC().Format(time.RFC3339Nano),
		RequestID:        strings.TrimSpace(internallogging.GetRequestID(ctx)),
		ConsumerName:     consumer,
		ProjectID:        fixedProjectID,
		Provider:         fixedProvider,
		ExecutorType:     strings.TrimSpace(record.ExecutorType),
		Model:            model,
		RequestModel:     model,
		Alias:            strings.TrimSpace(record.Alias),
		AuthID:           strings.TrimSpace(record.AuthID),
		AuthIndex:        strings.TrimSpace(record.AuthIndex),
		AuthType:         strings.TrimSpace(record.AuthType),
		Source:           strings.TrimSpace(record.Source),
		ReasoningEffort:  strings.TrimSpace(record.ReasoningEffort),
		ServiceTier:      strings.TrimSpace(record.ServiceTier),
		LatencyMs:        record.Latency.Milliseconds(),
		TTFTMs:           record.TTFT.Milliseconds(),
		Failed:           record.Failed,
		PromptTokens:     record.Detail.InputTokens,
		CompletionTokens: record.Detail.OutputTokens,
		UsageDetail:      usageDetailJSON,
	}
	if record.Failed {
		payload.Fail = &failDetail{
			StatusCode: record.Fail.StatusCode,
			Body:       strings.TrimSpace(record.Fail.Body),
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		log.WithError(err).Debug("usagelog: failed to marshal token usage record")
		return
	}

	if _, errStdout := fmt.Fprintln(os.Stdout, string(encoded)); errStdout != nil {
		log.WithError(errStdout).Debug("usagelog: failed to write token usage record to stdout")
	}

	if errWrite := p.writeLine(timestamp, encoded); errWrite != nil {
		log.WithError(errWrite).Debug("usagelog: failed to write token usage record")
	}
}

// writeLine appends a JSON line to the usage log file for the date derived
// from timestamp, rotating to a new file whenever the date changes.
func (p *plugin) writeLine(timestamp time.Time, line []byte) error {
	date := timestamp.UTC().Format(usageLogDateFormat)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.file == nil || p.fileDate != date {
		dir, err := usageLogDir()
		if err != nil {
			return err
		}
		if err = os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create usage log directory: %w", err)
		}
		path := filepath.Join(dir, date+".log")
		f, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if errOpen != nil {
			return fmt.Errorf("open usage log file: %w", errOpen)
		}
		if p.file != nil {
			if errClose := p.file.Close(); errClose != nil {
				log.WithError(errClose).Debug("usagelog: failed to close previous usage log file")
			}
		}
		p.file = f
		p.fileDate = date
	}

	if _, err := fmt.Fprintln(p.file, string(line)); err != nil {
		return err
	}
	return nil
}

// usageLogDir returns ~/.cli-proxy-api/usage.
func usageLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, usageLogBaseDir, usageLogSubdir), nil
}

// consumerUsername extracts the value of the X-Consumer-Username HTTP request
// header from the gin context attached to ctx.
func consumerUsername(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return ""
	}
	return strings.TrimSpace(ginCtx.Request.Header.Get(consumerUsernameHeader))
}

func hasReportableUsage(record coreusage.Record) bool {
	if record.Failed {
		return true
	}
	d := record.Detail
	return d.InputTokens != 0 ||
		d.OutputTokens != 0 ||
		d.ReasoningTokens != 0 ||
		d.CachedTokens != 0 ||
		d.CacheReadTokens != 0 ||
		d.CacheCreationTokens != 0 ||
		d.TotalTokens != 0
}

type tokenUsageLog struct {
	Timestamp        string      `json:"timestamp"`
	RequestID        string      `json:"request_id,omitempty"`
	ConsumerName     string      `json:"consumer_name,omitempty"`
	ProjectID        string      `json:"project_id"`
	Provider         string      `json:"provider"`
	ExecutorType     string      `json:"executor_type"`
	Model            string      `json:"model"`
	RequestModel     string      `json:"request_model"`
	Alias            string      `json:"alias,omitempty"`
	AuthID           string      `json:"auth_id,omitempty"`
	AuthIndex        string      `json:"auth_index,omitempty"`
	AuthType         string      `json:"auth_type,omitempty"`
	Source           string      `json:"source,omitempty"`
	ReasoningEffort  string      `json:"reasoning_effort,omitempty"`
	ServiceTier      string      `json:"service_tier,omitempty"`
	LatencyMs        int64       `json:"latency_ms"`
	TTFTMs           int64       `json:"ttft_ms"`
	Failed           bool        `json:"failed"`
	Fail             *failDetail `json:"fail,omitempty"`
	PromptTokens     int64       `json:"prompt_tokens"`
	CompletionTokens int64       `json:"completion_tokens"`
	UsageDetail      string      `json:"usage_detail"`
}

type failDetail struct {
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
}

type usageDetail struct {
	CompletionTokens        int64                   `json:"completion_tokens"`
	CompletionTokensDetails completionTokensDetails `json:"completion_tokens_details"`
	LatencyCheckpoint       latencyCheckpoint       `json:"latency_checkpoint"`
	PromptTokens            int64                   `json:"prompt_tokens"`
	PromptTokensDetails     promptTokensDetails     `json:"prompt_tokens_details"`
	TotalTokens             int64                   `json:"total_tokens"`
}

type completionTokensDetails struct {
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	AudioTokens              int64 `json:"audio_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens"`
}

type promptTokensDetails struct {
	AudioTokens  int64 `json:"audio_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
}

type latencyCheckpoint struct {
	EngineTBTMs       int64 `json:"engine_tbt_ms"`
	EngineTTFTMs      int64 `json:"engine_ttft_ms"`
	EngineTTLTMs      int64 `json:"engine_ttlt_ms"`
	PreInferenceMs    int64 `json:"pre_inference_ms"`
	ServiceTBTMs      int64 `json:"service_tbt_ms"`
	ServiceTTFTMs     int64 `json:"service_ttft_ms"`
	ServiceTTLTMs     int64 `json:"service_ttlt_ms"`
	TotalDurationMs   int64 `json:"total_duration_ms"`
	UserVisibleTTFTMs int64 `json:"user_visible_ttft_ms"`
}
