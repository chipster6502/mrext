```markdown
# Cliente Claude API - Arquitectura Robusta

## Archivo: cmd/remote/claude/client.go

```go
package claude

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
    "sync"
    
    "github.com/wizzomafizzo/mrext/pkg/config"
    "github.com/wizzomafizzo/mrext/pkg/service"
)

type Client struct {
    apiKey      string
    baseURL     string
    httpClient  *http.Client
    rateLimiter *RateLimiter
    logger      *service.Logger
}

type RateLimiter struct {
    requests    []time.Time
    maxRequests int
    window      time.Duration
    mutex       sync.Mutex
}

func NewClient(cfg *config.UserConfig, logger *service.Logger) *Client {
    return &Client{
        apiKey:  cfg.Claude.APIKey,
        baseURL: "https://api.anthropic.com/v1",
        httpClient: &http.Client{
            Timeout: time.Duration(cfg.Claude.TimeoutSeconds) * time.Second,
        },
        rateLimiter: NewRateLimiter(cfg.Claude.MaxRequestsPerHour, time.Hour),
        logger:      logger,
    }
}

func (c *Client) SendMessage(ctx context.Context, message string, gameContext *GameContext) (*ChatResponse, error) {
    // Verificar rate limiting
    if !c.rateLimiter.Allow() {
        return &ChatResponse{
            Error: "Rate limit exceeded. Please wait before making another request.",
            Timestamp: time.Now(),
        }, nil
    }

    // Construir prompt del sistema con contexto
    systemPrompt := c.buildSystemPrompt(gameContext)
    
    // Preparar petición Claude
    claudeReq := map[string]interface{}{
        "model":      "claude-3-sonnet-20240229",
        "max_tokens": 1024,
        "system":     systemPrompt,
        "messages": []map[string]string{
            {"role": "user", "content": message},
        },
    }
    
    // Enviar petición
    jsonData, _ := json.Marshal(claudeReq)
    httpReq, _ := http.NewRequestWithContext(ctx, "POST", 
        c.baseURL+"/messages", bytes.NewBuffer(jsonData))
    
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("x-api-key", c.apiKey)
    httpReq.Header.Set("anthropic-version", "2023-06-01")

    resp, err := c.httpClient.Do(httpReq)
    if err != nil {
        c.logger.Error("claude API request failed: %s", err)
        return nil, err
    }
    defer resp.Body.Close()

    // Procesar respuesta
    var claudeResp map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&claudeResp)
    
    return c.parseClaudeResponse(claudeResp), nil
}

func (c *Client) buildSystemPrompt(gameContext *GameContext) string {
    basePrompt := `Eres un asistente especializado en videojuegos retro y arcade para usuarios de MiSTer FPGA.
    
    Proporciona:
    - Consejos útiles y trucos para juegos
    - Curiosidades históricas sobre juegos y sistemas
    - Recomendaciones de juegos similares
    - Ayuda con problemas técnicos básicos
    
    Mantén respuestas concisas pero informativas.`
    
    if gameContext != nil && gameContext.GameName != "" {
        contextInfo := fmt.Sprintf(`
        
Contexto del usuario:
- Juego activo: %s
- Sistema: %s
- Core: %s

Adapta tus respuestas a este contexto cuando sea relevante.`,
            gameContext.GameName,
            gameContext.SystemName,
            gameContext.CoreName)
        
        basePrompt += contextInfo
    }
    
    return basePrompt
}