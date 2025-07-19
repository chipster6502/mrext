```markdown
# Tipos Claude - Siguiendo Convenciones Remote

## Archivo: cmd/remote/claude/types.go

```go
package claude

import "time"

// Siguiendo el patrón de PlayingPayload y ScreenshotPayload
type ChatRequest struct {
    Message        string `json:"message"`
    IncludeContext bool   `json:"include_context"`
    SessionID      string `json:"session_id,omitempty"`
}

type ChatResponse struct {
    Content   string       `json:"content"`
    Error     string       `json:"error,omitempty"`
    Timestamp time.Time    `json:"timestamp"`
    Context   *GameContext `json:"context,omitempty"`
}

type SuggestionsResponse struct {
    Suggestions []string     `json:"suggestions"`
    GameInfo    *GameContext `json:"game_info"`
    Generated   time.Time    `json:"generated"`
}

type PlaylistRequest struct {
    Description string   `json:"description"`
    MaxGames    int      `json:"max_games,omitempty"`
    Systems     []string `json:"systems,omitempty"`
}

type PlaylistResponse struct {
    Games       []GameEntry `json:"games"`
    Description string      `json:"description"`
    Generated   time.Time   `json:"generated"`
}

type GameEntry struct {
    Name   string `json:"name"`
    Path   string `json:"path"`
    System string `json:"system"`
}

// Contexto del juego usando información del Tracker
type GameContext struct {
    CoreName    string    `json:"core_name"`
    GameName    string    `json:"game_name"`
    SystemName  string    `json:"system_name"`
    GamePath    string    `json:"game_path"`
    LastStarted time.Time `json:"last_started"`
}
