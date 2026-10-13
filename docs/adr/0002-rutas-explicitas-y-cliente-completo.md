# 002 — Rutas explícitas `host → puerto` y un cliente que cubre toda la API

- **Fecha**: 2026-10-12
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

El ADR 001 hizo de pm un servicio con contrato: API v1, arriendos de puertos,
arranque por especificación, límites, dueños y un cliente Go. Su primer
consumidor externo, amnesia, cambió de forma al convertirse en producto
público: pm ya no es un requisito suyo sino una **integración opcional** que
mejora la experiencia en Linux. Eso le pide a pm dos cosas que hoy no tiene:

1. **Publicar un nombre para un puerto que pm no arrancó.** amnesia sirve sus
   entornos a través de su propio daemon (proxy por `Host`). Cuando pm y Caddy
   están, quiere además el nombre bonito en el puerto 80:
   `crb-144.localhost → 127.0.0.1:9810`. Hoy las rutas de Caddy nacen solo de
   las filas de `PS()` con estado «up» y del `Hostname()` del proyecto
   (`internal/daemon/daemon.go`, `reconcileProxy`). No hay forma de declarar
   una ruta suelta.
2. **Un cliente que cubra lo que la API ya ofrece.** `client/` tiene `Version`,
   `Run`, `Stop`, `Logs`, `Leases` y `Owners`, pero no `PS` (la foto de la
   máquina con atribución, que es la idea central de pm) ni `Detect`
   (`/api/v1/detect`). Un consumidor que quiera «¿de quién es este puerto?»
   tiene que llamar la API a mano.

Ninguna de las dos es específica de amnesia: una ruta suelta sirve para un
túnel, un contenedor ajeno o un servicio que pm no gestiona; `PS` y `Detect`
son la API que ya existe.

## Decisión

### Rutas explícitas

| Método | Ruta | Cuerpo / respuesta |
|---|---|---|
| POST | `/api/v1/routes` | `{host, port, owner?}` → `201 {host, port, owner}`; `409` si el host ya existe con otro puerto |
| GET | `/api/v1/routes` | `[{host, port, owner, source: explicit \| project}]` (incluye las derivadas de proyectos, marcadas) |
| DELETE | `/api/v1/routes/{host}` | `204`; solo rutas explícitas |

- Se guardan en el registry (`routes`), persisten y se reconcilian en el mismo
  ciclo que las de proyectos. Una explícita gana a una derivada con el mismo
  host.
- `DELETE /api/v1/owners/{owner}` borra también sus rutas (ADR 001, dueños).
- CLI: `pm route add <host> <port> [--owner o]`, `pm route rm <host>`,
  `pm route ls`. MCP: `pm_route_add`, `pm_route_rm`.
- Sin Caddy, la ruta se guarda igual y `GET /api/v1/version` sigue diciendo
  `caddy: false`; el consumidor decide.
- Eventos `route.*` en el SSE.

### Cliente completo

`client/` gana `PS(ctx) ([]Row, error)`, `Detect(ctx, dir) (Detected, error)`
y `Routes.Add/Remove/List`. Los tipos se duplican desde `openapi.yaml`, como
el resto del cliente. El test de contrato cliente↔daemon cubre las tres.

### Lo que no cambia

Nada de lo existente. Es la versión **0.2.1**.

## Plan de trabajo

| # | Paso | Hecho cuando |
|---|---|---|
| pm-12 | Rutas: tabla en el registry, rutas HTTP, reconciliación, CLI, MCP, eventos, `openapi.yaml`, tests (incluido «explícita gana a derivada» y «borrar dueño borra rutas») | `TestRoutesMatchOpenAPI` verde; `pm route add x.localhost 9810` aparece en el Caddyfile generado (test con fake de caddy) |
| pm-13 | Cliente: `PS`, `Detect`, `Routes`; `example_test.go` actualizado; README «Use pm from your tool» con `PS`; `CHANGELOG.md` con la entrada 0.2.1 (sin tag) | test de contrato cubre las tres llamadas; `go vet` del cliente sin dependencias |

## Consecuencias

- Cualquier herramienta puede darle un nombre `.localhost` a lo que sea,
  sin que pm lo gestione.
- El cliente deja de ser un subconjunto de la API.
- La guardia del ADR 001 sigue: estas dos funciones tienen uso propio desde
  la CLI; no hay vocabulario de amnesia en pm.
