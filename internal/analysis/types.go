// Package analysis 通过固定范围的正文工具运行可恢复、证据绑定的论文分析。
// 本包不访问文件系统、数据库或凭证。
package analysis

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

var ErrValidation = errors.New("analysis: invalid model output or evidence")

// Save 返回 nil 前必须持久化替换任务检查点。
// 请求前保存失败时禁止发送该请求。
type Save func(Checkpoint) error
type CheckpointSave = Save
type ChunkSave func(library.Chunk) error

// Checkpoint 仅属于一个任务 generation；额度和发送中占位跨重启保存。
// 提高 Engine 配置不能补充已开始任务的额度。
type Checkpoint struct {
	Run         library.Run                  `json:"run"`
	Sessions    map[string]SessionCheckpoint `json:"sessions"`
	Scope       string                       `json:"scope"`
	MaxRequests int                          `json:"max_requests"`
	MaxTokens   int64                        `json:"max_tokens"`
	StartedAt   string                       `json:"started_at"`
}
type SessionCheckpoint struct {
	History         []json.RawMessage `json:"history"`
	Phase           string            `json:"phase"`
	ReadBlocks      []string          `json:"read_blocks"`
	Attempts        int               `json:"attempts"`
	Result          json.RawMessage   `json:"result"`
	Run             library.Run       `json:"run"`
	Turns           []modelchat.Turn  `json:"turns"`
	FailedAssistant json.RawMessage   `json:"failed_assistant"`
}
type Engine struct {
	NewClient   func(*modelchat.Budget) (*modelchat.Client, error)
	Model       string
	MaxRequests int
	MaxTokens   int64
	Now         func() time.Time
}

// chunkContent 仅含模型生成字段；身份、读取标记和 hash 经校验后由程序补齐。
type chunkContent struct {
	Notes         []library.Claim    `json:"notes"`
	Evidence      []library.Evidence `json:"evidence"`
	MissingFields []string           `json:"missing_fields"`
}
