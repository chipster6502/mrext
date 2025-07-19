# Configuración Claude - Siguiendo Patrón UserConfig

## Estructura que añadiremos a pkg/config/user.go:

```go
type ClaudeConfig struct {
    Enabled           bool   `ini:"enabled,omitempty"`
    APIKey            string `ini:"api_key,omitempty"`
    Model             string `ini:"model,omitempty"`
    MaxRequestsPerHour int    `ini:"max_requests_per_hour,omitempty"`
    AutoSuggestions   bool   `ini:"auto_suggestions,omitempty"`
    ChatHistory       int    `ini:"chat_history,omitempty"`
    TimeoutSeconds    int    `ini:"timeout_seconds,omitempty"`
}

type UserConfig struct {
    // ... campos existentes
    Claude     ClaudeConfig     `ini:"claude,omitempty"`
}

[Archivo ini resultante:]: #

[claude]
enabled = false
api_key = 
model = claude-3-sonnet-20240229
max_requests_per_hour = 50
auto_suggestions = true
chat_history = 10
timeout_seconds = 30