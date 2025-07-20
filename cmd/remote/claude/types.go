package claude

import (
	"time"
)

// ✅ ADD: InstalledGame represents a game file found in the user's collection
type InstalledGame struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	System string `json:"system"`
}

// ChatRequest represents a user message sent to Claude
type ChatRequest struct {
	Message        string `json:"message"`              // User's question or message
	IncludeContext bool   `json:"include_context"`      // Whether to include current game context
	SessionID      string `json:"session_id,omitempty"` // Optional session for chat history
}

// ChatResponse contains Claude's reply and metadata
type ChatResponse struct {
	Content   string       `json:"content"`           // Claude's response text
	Error     string       `json:"error,omitempty"`   // Error message if request failed
	Timestamp time.Time    `json:"timestamp"`         // When response was generated
	Context   *GameContext `json:"context,omitempty"` // Game context used for response
}

// GameContext captures current MiSTer state from tracker
type GameContext struct {
	CoreName    string    `json:"core_name"`    // Active MiSTer core
	GameName    string    `json:"game_name"`    // Currently running game
	SystemName  string    `json:"system_name"`  // System/console name
	GamePath    string    `json:"game_path"`    // Full path to game file
	LastStarted time.Time `json:"last_started"` // When game was launched
}

// SuggestionsResponse provides automatic game suggestions
type SuggestionsResponse struct {
	Suggestions []string     `json:"suggestions"`       // List of suggestion texts
	Error       string       `json:"error,omitempty"`   // Error if suggestions failed
	Context     *GameContext `json:"context,omitempty"` // Current game context
	Timestamp   time.Time    `json:"timestamp"`         // Generation timestamp
}

// PlaylistRequest defines parameters for generating game playlists
type PlaylistRequest struct {
	Theme          string          `json:"theme"`                     // Playlist theme (e.g., "puzzle games")
	GameCount      int             `json:"game_count"`                // Number of games to recommend
	Systems        []string        `json:"systems,omitempty"`         // Specific systems to include
	Preferences    string          `json:"preferences,omitempty"`     // User preferences description
	InstalledGames []InstalledGame `json:"installed_games,omitempty"` // User's actual game collection
}

// PlaylistResponse contains generated game recommendations
type PlaylistResponse struct {
	Games     []GameRecommendation `json:"games"`           // List of recommended games
	Error     string               `json:"error,omitempty"` // Error if generation failed
	Theme     string               `json:"theme"`           // Theme used for generation
	Timestamp time.Time            `json:"timestamp"`       // Generation timestamp
}

// GameRecommendation represents a single game suggestion
type GameRecommendation struct {
	Name        string    `json:"name"`         // Game title
	System      string    `json:"system"`       // Target system/console
	Path        string    `json:"path"`         // Actual file path for launching
	Description string    `json:"description"`  // Brief game description
	Reason      string    `json:"reason"`       // Why this game was recommended
	GeneratedAt time.Time `json:"generated_at"` // When recommendation was made
	Theme       string    `json:"theme"`        // Theme used for generation
}

// AnthropicRequest follows Anthropic API specification
type AnthropicRequest struct {
	Model     string    `json:"model"`      // Claude model identifier
	MaxTokens int       `json:"max_tokens"` // Maximum response length
	Messages  []Message `json:"messages"`   // Conversation history
}

// AnthropicResponse contains API response from Claude
type AnthropicResponse struct {
	Content []struct {
		Text string `json:"text"`
		Type string `json:"type"`
	} `json:"content"`
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Message represents a single message in conversation
type Message struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"` // Message text
}

// ChatSession maintains conversation state
type ChatSession struct {
	ID       string    `json:"id"`       // Session identifier
	Messages []Message `json:"messages"` // Conversation history
	Created  time.Time `json:"created"`  // When session was created
	Updated  time.Time `json:"updated"`  // Last activity timestamp
}

// RateLimiter prevents API abuse
type RateLimiter struct {
	requests    []time.Time   // Timestamps of recent requests
	maxRequests int           // Maximum requests allowed
	window      time.Duration // Time window for rate limiting
}
