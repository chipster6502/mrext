package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wizzomafizzo/mrext/pkg/config"
	"github.com/wizzomafizzo/mrext/pkg/service"
	"github.com/wizzomafizzo/mrext/pkg/tracker"
)

const (
	anthropicAPIURL = "https://api.anthropic.com/v1/messages"
	maxTokens       = 1024
	userAgent       = "MiSTer-Remote-Claude/1.0"
)

type Client struct {
	config      *config.ClaudeConfig
	logger      *service.Logger
	httpClient  *http.Client
	rateLimiter *RateLimiter
	sessions    map[string]*ChatSession
	sessionMux  sync.RWMutex
}

func NewClient(cfg *config.ClaudeConfig, logger *service.Logger) *Client {
	return &Client{
		config: cfg,
		logger: logger,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
		rateLimiter: NewRateLimiter(cfg.MaxRequestsPerHour, time.Hour),
		sessions:    make(map[string]*ChatSession),
	}
}

func NewRateLimiter(maxRequests int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests:    make([]time.Time, 0),
		maxRequests: maxRequests,
		window:      window,
	}
}

func (r *RateLimiter) Allow() bool {
	now := time.Now()

	cutoff := now.Add(-r.window)
	validRequests := make([]time.Time, 0)
	for _, req := range r.requests {
		if req.After(cutoff) {
			validRequests = append(validRequests, req)
		}
	}
	r.requests = validRequests

	if len(r.requests) >= r.maxRequests {
		return false
	}

	r.requests = append(r.requests, now)
	return true
}

func (c *Client) SendMessage(ctx context.Context, message string, gameContext *GameContext, sessionID string) (*ChatResponse, error) {
	if !c.config.Enabled {
		return nil, fmt.Errorf("claude is disabled in configuration")
	}

	if !c.rateLimiter.Allow() {
		c.logger.Info("claude rate limit exceeded, request denied")
		return &ChatResponse{
			Error:     "Rate limit exceeded. Please try again later.",
			Timestamp: time.Now(),
		}, nil
	}

	prompt := c.buildPrompt(message, gameContext)
	session := c.getOrCreateSession(sessionID)

	session.Messages = append(session.Messages, Message{
		Role:    "user",
		Content: prompt,
	})

	c.trimSessionHistory(session)

	apiRequest := AnthropicRequest{
		Model:     c.config.Model,
		MaxTokens: maxTokens,
		Messages:  session.Messages,
	}

	apiResponse, err := c.callAnthropicAPI(ctx, &apiRequest)
	if err != nil {
		c.logger.Error("claude api call failed: %s", err)
		return &ChatResponse{
			Error:     "Failed to communicate with Claude. Please try again.",
			Timestamp: time.Now(),
			Context:   gameContext,
		}, nil
	}

	responseText := ""
	if len(apiResponse.Content) > 0 {
		responseText = apiResponse.Content[0].Text
	}

	session.Messages = append(session.Messages, Message{
		Role:    "assistant",
		Content: responseText,
	})
	session.Updated = time.Now()

	c.logger.Info("claude response generated successfully (tokens: %d/%d)",
		apiResponse.Usage.InputTokens, apiResponse.Usage.OutputTokens)

	return &ChatResponse{
		Content:   responseText,
		Timestamp: time.Now(),
		Context:   gameContext,
	}, nil
}

func (c *Client) GenerateSuggestions(ctx context.Context, trk *tracker.Tracker) (*SuggestionsResponse, error) {
	if !c.config.Enabled || !c.config.AutoSuggestions {
		return &SuggestionsResponse{
			Suggestions: []string{},
			Timestamp:   time.Now(),
		}, nil
	}

	gameContext := c.buildGameContext(trk)
	if gameContext.GameName == "" {
		return &SuggestionsResponse{
			Suggestions: []string{"No game currently running"},
			Timestamp:   time.Now(),
		}, nil
	}

	prompt := fmt.Sprintf(`Generate 3 brief, helpful suggestions for someone playing "%s" on the %s system. 
Focus on tips, strategies, or interesting facts. Keep each suggestion under 50 words.
Format as a simple list without numbers or bullets.`,
		gameContext.GameName, gameContext.SystemName)

	response, err := c.SendMessage(ctx, prompt, gameContext, "suggestions")
	if err != nil {
		return &SuggestionsResponse{
			Error:     "Failed to generate suggestions",
			Context:   gameContext,
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &SuggestionsResponse{
			Error:     response.Error,
			Context:   gameContext,
			Timestamp: time.Now(),
		}, nil
	}

	suggestions := c.parseSuggestions(response.Content)

	return &SuggestionsResponse{
		Suggestions: suggestions,
		Context:     gameContext,
		Timestamp:   time.Now(),
	}, nil
}

func (c *Client) GeneratePlaylist(ctx context.Context, request *PlaylistRequest) (*PlaylistResponse, error) {
	if !c.config.Enabled {
		return &PlaylistResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	prompt := c.buildPlaylistPrompt(request)

	response, err := c.SendMessage(ctx, prompt, nil, "playlist")
	if err != nil {
		return &PlaylistResponse{
			Error:     "Failed to generate playlist",
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &PlaylistResponse{
			Error:     response.Error,
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	games := c.parseGameRecommendations(response.Content, request.GameCount)

	return &PlaylistResponse{
		Games:     games,
		Theme:     request.Theme,
		Timestamp: time.Now(),
	}, nil
}

func (c *Client) buildPrompt(message string, gameContext *GameContext) string {
	if gameContext == nil || gameContext.GameName == "" {
		return fmt.Sprintf("You are Claude, an AI assistant integrated into MiSTer FPGA Remote. "+
			"Help the user with their question: %s", message)
	}

	return fmt.Sprintf(`You are Claude, an AI assistant integrated into MiSTer FPGA Remote.
Current context:
- Game: %s
- System: %s
- Core: %s

The user is currently playing this game and asks: %s

Provide helpful, relevant advice based on the game context.`,
		gameContext.GameName, gameContext.SystemName, gameContext.CoreName, message)
}

func (c *Client) buildGameContext(trk *tracker.Tracker) *GameContext {
	context := &GameContext{
		CoreName:    trk.ActiveCore,
		GameName:    trk.ActiveGameName,
		SystemName:  trk.ActiveSystemName,
		GamePath:    trk.ActiveGamePath,
		LastStarted: time.Now(),
	}

	if context.GameName == "" && context.CoreName != "" {
		if arcadeName := c.extractArcadeGameName(context.CoreName); arcadeName != "" {
			context.GameName = arcadeName
			context.SystemName = "Arcade"
			context.GamePath = ""
			c.logger.Info("claude: detected arcade core '%s', extracted game name '%s'", context.CoreName, arcadeName)
		}
	}

	return context
}

func (c *Client) extractArcadeGameName(coreName string) string {
	arcadeDir := "/media/fat/_Arcade"

	entries, err := os.ReadDir(arcadeDir)
	if err != nil {
		c.logger.Debug("claude: could not read _Arcade directory: %s", err)
		return ""
	}

	coreNameLower := strings.ToLower(coreName)
	bestMatch := ""
	bestScore := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !strings.HasSuffix(strings.ToLower(filename), ".mra") {
			continue
		}

		nameWithoutExt := strings.TrimSuffix(filename, ".mra")
		nameWithoutExtLower := strings.ToLower(nameWithoutExt)

		baseName := nameWithoutExt
		if idx := strings.Index(nameWithoutExt, "("); idx != -1 {
			baseName = strings.TrimSpace(nameWithoutExt[:idx])
		}
		baseNameLower := strings.ToLower(baseName)

		score := c.calculateMatchScore(coreNameLower, baseNameLower, nameWithoutExtLower)

		if score > bestScore {
			bestScore = score
			bestMatch = baseName
		}
	}

	if bestScore >= 70 {
		c.logger.Info("claude: matched core '%s' to arcade game '%s' (score: %d)", coreName, bestMatch, bestScore)
		return bestMatch
	}

	if c.isLikelyArcadeCore(coreName) {
		return c.cleanCoreName(coreName)
	}

	return ""
}

func (c *Client) calculateMatchScore(coreName, baseName, fullName string) int {
	score := 0

	if coreName == baseName {
		return 100
	}

	if strings.HasPrefix(coreName, baseName) {
		score += 80
	}

	if strings.HasPrefix(baseName, coreName) {
		score += 75
	}

	if strings.Contains(coreName, baseName) {
		score += 60
	}

	if strings.Contains(baseName, coreName) {
		score += 65
	}

	if len(coreName) == len(baseName)+1 && strings.HasPrefix(coreName, baseName) {
		score += 85
	}

	coreAbbrev := c.removeVowels(coreName)
	baseAbbrev := c.removeVowels(baseName)
	if coreAbbrev == baseAbbrev {
		score += 70
	}

	if c.similarStrings(coreName, baseName) {
		score += 50
	}

	return score
}

func (c *Client) removeVowels(s string) string {
	vowels := "aeiou"
	result := ""
	for _, char := range strings.ToLower(s) {
		if !strings.ContainsRune(vowels, char) {
			result += string(char)
		}
	}
	return result
}

func (c *Client) similarStrings(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}

	lengthDiff := len(a) - len(b)
	if lengthDiff < 0 {
		lengthDiff = -lengthDiff
	}

	if lengthDiff > 2 {
		return false
	}

	commonChars := 0
	shorter := a
	if len(b) < len(a) {
		shorter = b
	}

	for i := 0; i < len(shorter); i++ {
		if i < len(a) && i < len(b) && a[i] == b[i] {
			commonChars++
		}
	}

	return float64(commonChars)/float64(len(shorter)) >= 0.7
}

func (c *Client) isLikelyArcadeCore(coreName string) bool {
	knownConsoles := []string{
		"NES", "SNES", "Genesis", "SMS", "GG", "GB", "GBC", "GBA",
		"N64", "PSX", "Saturn", "32X", "SCD", "PCE", "TGFX16",
		"Atari2600", "Atari5200", "Atari7800", "ColecoVision",
		"Intellivision", "Vectrex", "Odyssey2", "Channelf",
		"A7800", "LYNX", "NGP", "WS", "C64", "Amiga", "Amstrad",
		"ZX", "MSX", "AppleII", "Macintosh",
	}

	for _, console := range knownConsoles {
		if strings.EqualFold(coreName, console) {
			return false
		}
	}

	arcadePatterns := []string{
		"194", "195", "196", "197", "198", "199",
		"pac", "kong", "man", "fighter", "force", "strike",
	}

	coreNameLower := strings.ToLower(coreName)
	for _, pattern := range arcadePatterns {
		if strings.Contains(coreNameLower, pattern) {
			return true
		}
	}

	return len(coreName) <= 10
}

func (c *Client) cleanCoreName(coreName string) string {
	name := strings.TrimSuffix(coreName, ".rbf")
	name = strings.TrimSuffix(name, "_MiSTer")

	words := strings.Fields(name)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.Title(strings.ToLower(word))
		}
	}

	return strings.Join(words, " ")
}

func (c *Client) callAnthropicAPI(ctx context.Context, request *AnthropicRequest) (*AnthropicResponse, error) {
	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.config.APIKey)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned status %d", resp.StatusCode)
	}

	var apiResponse AnthropicResponse
	err = json.NewDecoder(resp.Body).Decode(&apiResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &apiResponse, nil
}

func (c *Client) getOrCreateSession(sessionID string) *ChatSession {
	c.sessionMux.Lock()
	defer c.sessionMux.Unlock()

	if sessionID == "" {
		sessionID = fmt.Sprintf("session_%d", time.Now().Unix())
	}

	session, exists := c.sessions[sessionID]
	if !exists {
		session = &ChatSession{
			ID:       sessionID,
			Messages: make([]Message, 0),
			Created:  time.Now(),
			Updated:  time.Now(),
		}
		c.sessions[sessionID] = session
	}

	return session
}

func (c *Client) trimSessionHistory(session *ChatSession) {
	if len(session.Messages) > c.config.ChatHistory*2 {
		start := len(session.Messages) - c.config.ChatHistory*2
		session.Messages = session.Messages[start:]
	}
}

func (c *Client) parseSuggestions(content string) []string {
	var parts []string

	if strings.Contains(content, "\n\n") {
		parts = strings.Split(strings.TrimSpace(content), "\n\n")
	} else {
		parts = strings.Split(strings.TrimSpace(content), "\n")
	}

	suggestions := make([]string, 0)

	for _, part := range parts {
		part = strings.TrimSpace(part)

		if part == "" {
			continue
		}

		part = strings.TrimPrefix(part, "- ")
		part = strings.TrimPrefix(part, "• ")
		part = strings.TrimPrefix(part, "* ")
		part = strings.TrimPrefix(part, "1. ")
		part = strings.TrimPrefix(part, "2. ")
		part = strings.TrimPrefix(part, "3. ")

		part = strings.TrimSpace(part)

		if len(part) >= 20 && len(part) <= 300 {
			suggestions = append(suggestions, part)
		}

		if len(suggestions) >= 3 {
			break
		}
	}

	if len(suggestions) > 0 {
		c.logger.Info("claude: parsed %d suggestions successfully", len(suggestions))
		return suggestions
	}

	c.logger.Warn("claude: suggestion parsing failed, using fallback")
	return []string{
		"Try exploring different strategies",
		"Check for hidden mechanics or features",
		"Practice timing and precision",
	}
}

func (c *Client) buildPlaylistPrompt(request *PlaylistRequest) string {
	prompt := fmt.Sprintf("Generate exactly %d game recommendations for the theme: %s\n",
		request.GameCount, request.Theme)

	if len(request.Systems) > 0 {
		prompt += fmt.Sprintf("Focus on these systems: %v\n", request.Systems)
	}

	if request.Preferences != "" {
		prompt += fmt.Sprintf("User preferences: %s\n", request.Preferences)
	}

	prompt += "Format each game as: Game Name | System | Brief description | Why recommended"

	return prompt
}

func (c *Client) parseGameRecommendations(content string, count int) []GameRecommendation {
	recommendations := make([]GameRecommendation, 0, count)

	// Split by double newlines first, then single newlines as backup
	var lines []string
	if strings.Contains(content, "\n\n") {
		parts := strings.Split(content, "\n\n")
		// Take each part and split by single newlines too
		for _, part := range parts {
			subLines := strings.Split(strings.TrimSpace(part), "\n")
			lines = append(lines, subLines...)
		}
	} else {
		lines = strings.Split(strings.TrimSpace(content), "\n")
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines or lines that don't contain pipes
		if line == "" || !strings.Contains(line, "|") {
			continue
		}

		// Skip lines that look like headers or intro text
		if strings.Contains(strings.ToLower(line), "recommendations") ||
			strings.Contains(strings.ToLower(line), "here are") {
			continue
		}

		// Split by pipe character
		parts := strings.Split(line, "|")

		// Should have exactly 4 parts: Name | System | Description | Reason
		if len(parts) != 4 {
			continue
		}

		// Clean up each part
		name := strings.TrimSpace(parts[0])
		system := strings.TrimSpace(parts[1])
		description := strings.TrimSpace(parts[2])
		reason := strings.TrimSpace(parts[3])

		// Validate that we have meaningful content
		if len(name) < 2 || len(system) < 2 || len(description) < 10 {
			continue
		}

		// Clean up "Recommended for" prefix if present
		if strings.HasPrefix(strings.ToLower(reason), "recommended for ") {
			reason = strings.TrimPrefix(reason, "Recommended for ")
			reason = strings.TrimPrefix(reason, "recommended for ")
		}

		recommendations = append(recommendations, GameRecommendation{
			Name:        name,
			System:      system,
			Description: description,
			Reason:      reason,
		})

		// Stop when we reach the requested count
		if len(recommendations) >= count {
			break
		}
	}

	// If we got good recommendations, return them
	if len(recommendations) > 0 {
		c.logger.Info("claude: parsed %d game recommendations successfully", len(recommendations))
		return recommendations
	}

	// Fallback only if parsing completely failed
	c.logger.Warn("claude: game recommendation parsing failed, using fallback")
	return []GameRecommendation{
		{
			Name:        "Pac-Man",
			System:      "Arcade",
			Description: "Classic maze game with dots and ghosts",
			Reason:      "Timeless gameplay and universal appeal",
		},
		{
			Name:        "Space Invaders",
			System:      "Arcade",
			Description: "Defend Earth from waves of alien invaders",
			Reason:      "Pioneering shoot-em-up with escalating difficulty",
		},
		{
			Name:        "Asteroids",
			System:      "Arcade",
			Description: "Navigate space while destroying asteroids and UFOs",
			Reason:      "Innovative vector graphics and physics-based gameplay",
		},
	}
}
