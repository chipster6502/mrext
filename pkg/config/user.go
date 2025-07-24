package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/ini.v1"
)

type LaunchSyncConfig struct{}

type PlayLogConfig struct {
	SaveEvery   int    `ini:"save_every,omitempty"`
	OnCoreStart string `ini:"on_core_start,omitempty"`
	OnCoreStop  string `ini:"on_core_stop,omitempty"`
	OnGameStart string `ini:"on_game_start,omitempty"`
	OnGameStop  string `ini:"on_game_stop,omitempty"`
}

type RandomConfig struct{}

type SearchConfig struct {
	Filter []string `ini:"filter,omitempty" delim:","`
	Sort   string   `ini:"sort,omitempty"`
}

type LastPlayedConfig struct {
	Name                string `ini:"name,omitempty"`
	LastPlayedName      string `ini:"last_played_name,omitempty"`
	DisableLastPlayed   bool   `ini:"disable_last_played,omitempty"`
	RecentFolderName    string `ini:"recent_folder_name,omitempty"`
	DisableRecentFolder bool   `ini:"disable_recent_folder,omitempty"`
}

// RemoteConfig holds configuration for the Remote web server
type RemoteConfig struct {
	MdnsService     bool   `ini:"mdns_service,omitempty"`
	SyncSSHKeys     bool   `ini:"sync_ssh_keys,omitempty"`
	CustomLogo      string `ini:"custom_logo,omitempty"`
	AnnounceGameUrl string `ini:"announce_game_url,omitempty"`
	// ✅ NEW: Host configuration for frontend
	Host string `ini:"host,omitempty"` // IP or hostname of MiSTer (e.g: "192.168.1.222")
	Port int    `ini:"port,omitempty"` // Server port (default: 8182)
}

type NfcConfig struct {
	ConnectionString string `ini:"connection_string,omitempty"`
	AllowCommands    bool   `ini:"allow_commands,omitempty"`
	DisableSounds    bool   `ini:"disable_sounds,omitempty"`
	ProbeDevice      bool   `ini:"probe_device,omitempty"`
}

type SystemsConfig struct {
	GamesFolder []string `ini:"games_folder,omitempty,allowshadow"`
	SetCore     []string `ini:"set_core,omitempty,allowshadow"`
}

type UserConfig struct {
	AppPath    string
	IniPath    string
	LaunchSync LaunchSyncConfig `ini:"launchsync,omitempty"`
	PlayLog    PlayLogConfig    `ini:"playlog,omitempty"`
	Random     RandomConfig     `ini:"random,omitempty"`
	Search     SearchConfig     `ini:"search,omitempty"`
	LastPlayed LastPlayedConfig `ini:"lastplayed,omitempty"`
	Remote     RemoteConfig     `ini:"remote,omitempty"`
	Nfc        NfcConfig        `ini:"nfc,omitempty"`
	Systems    SystemsConfig    `ini:"systems,omitempty"`
	Claude     ClaudeConfig     `ini:"claude,omitempty"`
}

// ClaudeConfig holds configuration for Claude AI integration
type ClaudeConfig struct {
	Enabled            bool   `ini:"enabled,omitempty"`               // Master switch for Claude functionality
	APIKey             string `ini:"api_key,omitempty"`               // Anthropic API key
	Model              string `ini:"model,omitempty"`                 // Claude model to use
	MaxRequestsPerHour int    `ini:"max_requests_per_hour,omitempty"` // Rate limiting for cost control
	AutoSuggestions    bool   `ini:"auto_suggestions,omitempty"`      // Enable automatic game suggestions
	ChatHistory        int    `ini:"chat_history,omitempty"`          // Number of messages to keep in memory
	TimeoutSeconds     int    `ini:"timeout_seconds,omitempty"`       // HTTP timeout for API requests
}

func LoadUserConfig(name string, defaultConfig *UserConfig) (*UserConfig, error) {
	iniPath := os.Getenv(UserConfigEnv)

	exePath, err := os.Executable()
	if err != nil {
		return defaultConfig, err
	}

	appPath := os.Getenv(UserAppPathEnv)
	if appPath != "" {
		exePath = appPath
	}

	if iniPath == "" {
		iniPath = filepath.Join(filepath.Dir(exePath), name+".ini")
	}

	defaultConfig.AppPath = exePath
	defaultConfig.IniPath = iniPath

	if _, err := os.Stat(iniPath); os.IsNotExist(err) {
		return defaultConfig, nil
	}

	cfg, err := ini.ShadowLoad(iniPath)
	if err != nil {
		return defaultConfig, err
	}

	err = cfg.StrictMapTo(defaultConfig)
	if err != nil {
		return defaultConfig, err
	}

	return defaultConfig, nil
}

// GetDefaultClaudeConfig returns safe default configuration values
func GetDefaultClaudeConfig() ClaudeConfig {
	return ClaudeConfig{
		Enabled:            false,                        // Disabled by default for security
		APIKey:             "",                           // User must provide their own key
		Model:              "claude-3-5-sonnet-20241022", // Balanced cost/performance model
		MaxRequestsPerHour: 50,                           // Conservative limit
		AutoSuggestions:    true,
		ChatHistory:        10,
		TimeoutSeconds:     30,
	}
}

// ValidateClaudeConfig validates configuration values
func (c *ClaudeConfig) ValidateClaudeConfig() error {
	if c.Enabled {
		if c.APIKey == "" {
			return fmt.Errorf("claude enabled but no API key provided")
		}
		if c.MaxRequestsPerHour <= 0 {
			return fmt.Errorf("max_requests_per_hour must be greater than 0")
		}
		if c.TimeoutSeconds <= 0 {
			return fmt.Errorf("timeout_seconds must be greater than 0")
		}
		if c.ChatHistory < 0 {
			return fmt.Errorf("chat_history cannot be negative")
		}
	}
	return nil
}

// GetDefaultRemoteConfig returns safe default configuration values
func GetDefaultRemoteConfig() RemoteConfig {
	return RemoteConfig{
		MdnsService:     true,
		SyncSSHKeys:     true,
		CustomLogo:      "",
		AnnounceGameUrl: "",
		Host:            "localhost", // Default for development
		Port:            8182,        // Standard Remote port
	}
}
