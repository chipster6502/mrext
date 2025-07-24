package settings

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wizzomafizzo/mrext/pkg/config"
	"github.com/wizzomafizzo/mrext/pkg/service"
)

// FrontendConfigResponse holds configuration data needed by the frontend
type FrontendConfigResponse struct {
	Host   string `json:"host"`    // MiSTer host/IP
	Port   int    `json:"port"`    // MiSTer port
	ApiUrl string `json:"api_url"` // Complete API URL
}

// HandleFrontendConfig provides configuration needed by the frontend
func HandleFrontendConfig(logger *service.Logger, cfg *config.UserConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Build API URL from configuration
		apiUrl := ""
		if cfg.Remote.Host != "" && cfg.Remote.Port > 0 {
			if cfg.Remote.Host == "localhost" || cfg.Remote.Host == "127.0.0.1" {
				// Para desarrollo local, usar la IP actual del sistema
				apiUrl = "" // El frontend usará URL relativa
			} else {
				apiUrl = fmt.Sprintf("http://%s:%d", cfg.Remote.Host, cfg.Remote.Port)
			}
		}

		response := FrontendConfigResponse{
			Host:   cfg.Remote.Host,
			Port:   cfg.Remote.Port,
			ApiUrl: apiUrl,
		}

		logger.Info("frontend config: serving %s:%d", response.Host, response.Port)

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("frontend config: failed to encode response: %s", err)
			return
		}
	}
}
