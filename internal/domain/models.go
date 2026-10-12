package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	AuthTypeBearer = "bearer" // Authorization: Bearer <key>
	AuthTypeHeader = "header" // custom header (e.g. x-api-key)
	AuthTypeNone   = "none"   // no auth

	ModeToken = "token" // quota counted in tokens, deducted by usage
	ModeCalls = "calls" // quota counted in call count
)

// User is a platform user (account that owns one or more API tokens).
type User struct {
	ID          bson.ObjectID `bson:"_id,omitempty" json:"id"`
	Name        string        `bson:"name" json:"name"`
	Enabled     bool          `bson:"enabled" json:"enabled"`
	Description string        `bson:"description" json:"description"`
	CreatedAt   time.Time     `bson:"createdAt" json:"createdAt"`
}

// Token is an end-user API key used to call the public OpenAI-compatible API.
// Only the hash is stored; the plaintext is returned once on creation.
type Token struct {
	ID         bson.ObjectID `bson:"_id,omitempty" json:"id"`
	UserID     bson.ObjectID `bson:"userId" json:"userId"`
	Name       string        `bson:"name" json:"name"`
	KeyHash    string        `bson:"keyHash" json:"-"`
	Enabled    bool          `bson:"enabled" json:"enabled"`
	LastUsedAt *time.Time    `bson:"lastUsedAt,omitempty" json:"lastUsedAt"`
	CreatedAt  time.Time     `bson:"createdAt" json:"createdAt"`
}

// Channel is an upstream OpenAI-compatible provider.
type Channel struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id"`
	Name         string        `bson:"name" json:"name"`
	BaseURL      string        `bson:"baseUrl" json:"baseUrl"` // e.g. https://api.openai.com/v1
	AuthType     string        `bson:"authType" json:"authType"`
	APIKeyHeader string        `bson:"apiKeyHeader,omitempty" json:"apiKeyHeader,omitempty"` // header name when authType=header
	TimeoutSec   int           `bson:"timeoutSec" json:"timeoutSec"`                         // per-request timeout, 0 = default
	Enabled      bool          `bson:"enabled" json:"enabled"`
	CreatedAt    time.Time     `bson:"createdAt" json:"createdAt"`
}

// ChannelKey is one API key belonging to a channel. Keys rotate round-robin.
type ChannelKey struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id"`
	ChannelID bson.ObjectID `bson:"channelId" json:"channelId"`
	Key       string        `bson:"key" json:"key"`
	Enabled   bool          `bson:"enabled" json:"enabled"`
	CreatedAt time.Time     `bson:"createdAt" json:"createdAt"`
}

// Quota tracks the remaining quota of one model on one channel.
// Mode token: Remaining is in tokens, deducted by usage (with optional multiplier).
// Mode calls: Remaining is a call count, deducted by ChargePerCall (images: ChargePerCall per image).
type Quota struct {
	ID            bson.ObjectID `bson:"_id,omitempty" json:"id"`
	ChannelID     bson.ObjectID `bson:"channelId" json:"channelId"`
	Model         string        `bson:"model" json:"model"`
	Mode          string        `bson:"mode" json:"mode"`
	Remaining     float64       `bson:"remaining" json:"remaining"`         // remaining tokens or calls
	Reserved      float64       `bson:"reserved" json:"reserved"`           // in-flight reserved amount (unbilled)
	ChargePerCall float64       `bson:"chargePerCall" json:"chargePerCall"` // extra charge per call/image for ModeCalls (default 1)
	Multiplier    float64       `bson:"multiplier" json:"multiplier"`       // consumption coefficient for ModeToken (default 1)
	Enabled       bool          `bson:"enabled" json:"enabled"`
	Note          string        `bson:"note,omitempty" json:"note"`
	CreatedAt     time.Time     `bson:"createdAt" json:"createdAt"`
	UpdatedAt     time.Time     `bson:"updatedAt" json:"updatedAt"`
}

// Candidate is one route entry: which channel+model to try, in priority order.
type Candidate struct {
	ChannelID bson.ObjectID `bson:"channelId" json:"channelId"`
	Model     string        `bson:"model" json:"model"`
	Priority  int           `bson:"priority" json:"priority"` // lower = tried first
}

// VirtualModel is the model name exposed to end users.
type VirtualModel struct {
	ID          bson.ObjectID `bson:"_id,omitempty" json:"id"`
	Name        string        `bson:"name" json:"name"`
	Description string        `bson:"description,omitempty" json:"description"`
	Candidates  []Candidate   `bson:"candidates" json:"candidates"`
	Enabled     bool          `bson:"enabled" json:"enabled"`
	CreatedAt   time.Time     `bson:"createdAt" json:"createdAt"`
	UpdatedAt   time.Time     `bson:"updatedAt" json:"updatedAt"`
}

// RequestLog records every public API call.
type RequestLog struct {
	ID               bson.ObjectID `bson:"_id,omitempty" json:"id"`
	UserID           bson.ObjectID `bson:"userId" json:"userId"`
	TokenID          bson.ObjectID `bson:"tokenId" json:"tokenId"`
	Endpoint         string        `bson:"endpoint" json:"endpoint"` // /v1/chat/completions | /v1/images/generations
	VirtualModel     string        `bson:"virtualModel" json:"virtualModel"`
	ChannelID        bson.ObjectID `bson:"channelId,omitempty" json:"channelId"` // actually used channel
	ChannelName      string        `bson:"channelName,omitempty" json:"channelName"`
	ActualModel      string        `bson:"actualModel,omitempty" json:"actualModel"`
	Mode             string        `bson:"mode,omitempty" json:"mode"`
	PromptTokens     int64         `bson:"promptTokens" json:"promptTokens"`
	CompletionTokens int64         `bson:"completionTokens" json:"completionTokens"`
	TotalTokens      int64         `bson:"totalTokens" json:"totalTokens"`
	Images           int           `bson:"images" json:"images"`
	Charge           float64       `bson:"charge" json:"charge"` // quota consumed
	Status           string        `bson:"status" json:"status"` // success | error
	Error            string        `bson:"error,omitempty" json:"error"`
	LatencyMs        int64         `bson:"latencyMs" json:"latencyMs"`
	CreatedAt        time.Time     `bson:"createdAt" json:"createdAt"`
}

// Settings holds key/value configuration (admin credentials, secrets, defaults).
type Settings struct {
	ID    bson.ObjectID `bson:"_id,omitempty" json:"id"`
	Key   string        `bson:"key" json:"key"`
	Value string        `bson:"value" json:"value"`
}
