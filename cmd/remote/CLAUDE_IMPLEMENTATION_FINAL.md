```markdown
# Plan de Implementación Claude - Basado en Código Real

## Fase 1: Configuración (30 minutos)

### Archivo 1: pkg/config/user.go
- [ ] Añadir `ClaudeConfig` struct después de `RemoteConfig`
- [ ] Añadir `Claude ClaudeConfig` a `UserConfig`
- [ ] Seguir exactamente el patrón de `RemoteConfig`

### Verificación:
```bash
# Probar que la configuración se carga correctamente
go build cmd/remote/main.go


# Fase 2: Tipos de Datos (45 minutos)
Archivo 2: cmd/remote/claude/types.go

 Crear paquete claude
 Definir todos los tipos (ChatRequest, ChatResponse, etc.)
 Asegurar compatibilidad JSON para frontend

# Fase 3: Cliente API (2 horas)
Archivo 3: cmd/remote/claude/client.go

 Implementar cliente HTTP para Anthropic
 Añadir rate limiting robusto
 Integrar con sistema de logging de Remote
 Manejar errores siguiendo patrón de Remote

# Fase 4: Handlers HTTP (1 hora)
Archivo 4: cmd/remote/claude/handlers.go

 Implementar HandleChat siguiendo patrón screenshots
 Implementar HandleSuggestions
 Implementar HandlePlaylist
 Integrar con tracker para contexto automático

# Fase 5: Integración Main (15 minutos)
Archivo 5: cmd/remote/main.go

 Añadir import claude
 Añadir rutas en setupApi
 Probar compilación completa

Archivos a crear:

cmd/remote/claude/types.go
cmd/remote/claude/client.go
cmd/remote/claude/handlers.go

Archivos a modificar:

pkg/config/user.go (añadir ClaudeConfig)
cmd/remote/main.go (añadir rutas)

Testing paso a paso:

Configuración: verificar que .ini carga correctamente
Cliente: probar conexión con API de Anthropic
Handlers: probar endpoints con curl
Integración: probar desde frontend (cuando esté listo)
