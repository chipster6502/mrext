# Handlers Claude - Siguiendo Patrón Screenshots

## Archivo: cmd/remote/claude/handlers.go

```go
package claude

import (
    "encoding/json"
    "net/http"
    
    "github.com/wizzomafizzo/mrext/pkg/service"
    "github.com/wizzomafizzo/mrext/pkg/config"
    "github.com/wizzomafizzo/mrext/pkg/tracker"
)

// Siguiendo el patrón exacto de screenshots
func HandleChat(logger *service.Logger, cfg *config.UserConfig, tr *tracker.Tracker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Verificar si Claude está habilitado
        if !cfg.Claude.Enabled {
            http.Error(w, "Claude not enabled", http.StatusServiceUnavailable)
            return
        }
        
        // Decodificar petición
        var request ChatRequest
        err := json.NewDecoder(r.Body).Decode(&request)
        if err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            logger.Error("claude chat: decoding request: %s", err)
            return
        }
        
        // Crear contexto del juego usando el tracker
        gameContext := &GameContext{
            CoreName:    tr.ActiveCore,
            GameName:    tr.ActiveGameName,
            SystemName:  tr.ActiveSystemName,
            GamePath:    tr.ActiveGamePath,
        }
        
        // Enviar a Claude API
        response, err := sendToClaudeAPI(cfg, request, gameContext)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            logger.Error("claude chat: API error: %s", err)
            return
        }
        
        // Devolver respuesta
        err = json.NewEncoder(w).Encode(response)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            logger.Error("claude chat: encoding response: %s", err)
            return
        }
    }
}

func HandleSuggestions(logger *service.Logger, cfg *config.UserConfig, tr *tracker.Tracker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Solo si hay un juego activo
        if tr.ActiveGame == "" {
            http.Error(w, "No active game", http.StatusBadRequest)
            return
        }
        
        // Generar sugerencias automáticas para el juego actual
        // ... implementación similar
    }
}

[Integración en main.go setupApi:]: #

// En setupApi, añadir después de las rutas existentes:
sub.HandleFunc("/claude/chat", claude.HandleChat(logger, cfg, trk)).Methods("POST")
sub.HandleFunc("/claude/suggestions", claude.HandleSuggestions(logger, cfg, trk)).Methods("GET")
sub.HandleFunc("/claude/playlist", claude.HandlePlaylist(logger, cfg, trk)).Methods("POST")