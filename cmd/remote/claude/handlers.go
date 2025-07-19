package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/wizzomafizzo/mrext/pkg/config"
	"github.com/wizzomafizzo/mrext/pkg/service"
	"github.com/wizzomafizzo/mrext/pkg/tracker"
)

// HandleChat processes interactive chat requests with Claude
func HandleChat(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Validate API key
		if cfg.Claude.APIKey == "" {
			http.Error(w, "Claude API key not configured", http.StatusServiceUnavailable)
			return
		}

		// Parse request
		var request ChatRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			logger.Error("claude chat: failed to decode request: %s", err)
			return
		}

		// Validate message
		if request.Message == "" {
			http.Error(w, "Message cannot be empty", http.StatusBadRequest)
			return
		}

		logger.Info("claude chat: processing message from session %s", request.SessionID)

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Get game context if requested
		var gameContext *GameContext
		if request.IncludeContext && trk != nil {
			gameContext = client.buildGameContext(trk)
		}

		// Create context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Claude.TimeoutSeconds)*time.Second)
		defer cancel()

		// Send message to Claude
		response, err := client.SendMessage(ctx, request.Message, gameContext, request.SessionID)
		if err != nil {
			logger.Error("claude chat: %s", err)
			http.Error(w, "Failed to process chat request", http.StatusInternalServerError)
			return
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude chat: failed to encode response: %s", err)
			return
		}

		logger.Info("claude chat: response sent successfully")
	}
}

// HandleSuggestions generates automatic game suggestions
func HandleSuggestions(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Check if auto suggestions are enabled
		if !cfg.Claude.AutoSuggestions {
			response := &SuggestionsResponse{
				Suggestions: []string{},
				Timestamp:   time.Now(),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}

		// Validate API key
		if cfg.Claude.APIKey == "" {
			http.Error(w, "Claude API key not configured", http.StatusServiceUnavailable)
			return
		}

		logger.Info("claude suggestions: generating for current game")

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Create context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Claude.TimeoutSeconds)*time.Second)
		defer cancel()

		// Generate suggestions
		response, err := client.GenerateSuggestions(ctx, trk)
		if err != nil {
			logger.Error("claude suggestions: %s", err)
			http.Error(w, "Failed to generate suggestions", http.StatusInternalServerError)
			return
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude suggestions: failed to encode response: %s", err)
			return
		}

		logger.Info("claude suggestions: %d suggestions generated", len(response.Suggestions))
	}
}

// HandlePlaylist generates themed game playlists
func HandlePlaylist(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Validate API key
		if cfg.Claude.APIKey == "" {
			http.Error(w, "Claude API key not configured", http.StatusServiceUnavailable)
			return
		}

		// Parse request
		var request PlaylistRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			logger.Error("claude playlist: failed to decode request: %s", err)
			return
		}

		// Validate request
		if request.Theme == "" {
			http.Error(w, "Theme cannot be empty", http.StatusBadRequest)
			return
		}

		if request.GameCount <= 0 || request.GameCount > 20 {
			request.GameCount = 5 // Default to 5 games
		}

		logger.Info("claude playlist: generating %d games for theme '%s'", request.GameCount, request.Theme)

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Create context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Claude.TimeoutSeconds)*time.Second)
		defer cancel()

		// Generate playlist
		response, err := client.GeneratePlaylist(ctx, &request)
		if err != nil {
			logger.Error("claude playlist: %s", err)
			http.Error(w, "Failed to generate playlist", http.StatusInternalServerError)
			return
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude playlist: failed to encode response: %s", err)
			return
		}

		logger.Info("claude playlist: %d games generated for theme '%s'", len(response.Games), request.Theme)
	}
}

// HandleStatus provides Claude configuration status
func HandleStatus(logger *service.Logger, cfg *config.UserConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := map[string]interface{}{
			"enabled":          cfg.Claude.Enabled,
			"api_key_set":      cfg.Claude.APIKey != "",
			"model":            cfg.Claude.Model,
			"auto_suggestions": cfg.Claude.AutoSuggestions,
			"max_requests":     cfg.Claude.MaxRequestsPerHour,
			"chat_history":     cfg.Claude.ChatHistory,
			"timeout":          cfg.Claude.TimeoutSeconds,
		}

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(status)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude status: failed to encode response: %s", err)
			return
		}
	}
}

// HandleUpdateConfig allows runtime configuration updates
func HandleUpdateConfig(logger *service.Logger, cfg *config.UserConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse configuration updates
		var updates map[string]interface{}
		err := json.NewDecoder(r.Body).Decode(&updates)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			logger.Error("claude config update: failed to decode request: %s", err)
			return
		}

		// Apply safe configuration updates
		updated := false

		if enabled, ok := updates["enabled"].(bool); ok {
			cfg.Claude.Enabled = enabled
			updated = true
			logger.Info("claude config: enabled set to %t", enabled)
		}

		if autoSugg, ok := updates["auto_suggestions"].(bool); ok {
			cfg.Claude.AutoSuggestions = autoSugg
			updated = true
			logger.Info("claude config: auto_suggestions set to %t", autoSugg)
		}

		if maxReq, ok := updates["max_requests_per_hour"].(float64); ok && maxReq > 0 {
			cfg.Claude.MaxRequestsPerHour = int(maxReq)
			updated = true
			logger.Info("claude config: max_requests_per_hour set to %d", int(maxReq))
		}

		if chatHist, ok := updates["chat_history"].(float64); ok && chatHist >= 0 {
			cfg.Claude.ChatHistory = int(chatHist)
			updated = true
			logger.Info("claude config: chat_history set to %d", int(chatHist))
		}

		if timeout, ok := updates["timeout_seconds"].(float64); ok && timeout > 0 {
			cfg.Claude.TimeoutSeconds = int(timeout)
			updated = true
			logger.Info("claude config: timeout_seconds set to %d", int(timeout))
		}

		// Validate updated configuration
		if updated {
			err = cfg.Claude.ValidateClaudeConfig()
			if err != nil {
				http.Error(w, "Invalid configuration: "+err.Error(), http.StatusBadRequest)
				logger.Error("claude config update: validation failed: %s", err)
				return
			}
		}

		// Return success response
		response := map[string]interface{}{
			"success": updated,
			"message": func() string {
				if updated {
					return "Configuration updated successfully"
				}
				return "No valid updates provided"
			}(),
		}

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude config update: failed to encode response: %s", err)
			return
		}

		if updated {
			logger.Info("claude config: configuration updated successfully")
		}
	}
}

// Helper function to parse integer query parameters
func parseIntParam(r *http.Request, param string, defaultValue int) int {
	value := r.URL.Query().Get(param)
	if value == "" {
		return defaultValue
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}

	return parsed
}
