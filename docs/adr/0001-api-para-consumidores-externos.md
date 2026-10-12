# 001 — pm es la capa de procesos y puertos de otras herramientas: API v1 versionada y cliente oficial

- **Fecha**: 2026-10-12
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

pm nació como herramienta personal: un binario que sabe qué corre en qué
puerto en una máquina WSL2, arranca y para proyectos bajo `systemd --user`, y
los nombra `<proyecto>.localhost` vía Caddy. Lo usan una persona y sus agentes
(CLI, web, API HTTP, MCP).

Apareció un segundo consumidor real: **amnesia**, un tablero de desarrollo que
levanta entornos de prueba por tarjeta. amnesia decidió **no repartir puertos
ni lanzar procesos por su cuenta**: se lo pide a pm. La razón es la misma idea
fundacional de pm: en una máquina debe haber un solo dueño de los puertos, o
dos herramientas terminan pisándose.

Se evaluó la alternativa de exportar los paquetes internos de pm como librería
Go. Se descartó: obligaría a mantener dos formas de integrar el mismo producto
(paquetes y API) y amnesia necesita de todos modos que pm esté vivo para que
la atribución de puertos sea de toda la máquina, no de un proceso.

Tres hechos del código actual, verificados 2026-10-12, condicionan el plan:

1. **No hay límites de recursos.** `systemd-run` se invoca sin `MemoryMax` ni
   `CPUQuota` (`internal/runner/runner.go:133-153`).
2. **Son servicios transitorios, no scopes.** `systemd-run` va sin `--scope`.
   El README y `internal/registry/model.go:8` siguen diciendo «transient
   scope».
3. **La API HTTP no está versionada ni documentada**
   (`internal/daemon/handlers.go:17-40`: `/api/projects`, `/api/claim`,
   `/api/detect`, …). Solo arranca proyectos **registrados** por nombre
   (`runner.Start` exige `Store.Get`), con `LaunchOpts{Port, Force}` como
   única parametrización. No hay forma de arrancar algo con un comando,
   variables y límites dados en la petición, ni de reservar puertos sin
   arrancar nada.

## Decisión

### pm sigue siendo un producto para una persona y sus agentes

Nada de lo que sigue se diseña para amnesia. Cada función tiene un uso desde
la CLI y el MCP para alguien que no sabe qué es amnesia. **Regla:** si una
petición de un consumidor solo tiene sentido en su vocabulario (tarjeta,
workspace), se queda en el consumidor. pm habla de proyectos, procesos,
puertos, grupos y dueños.

### API v1, versionada y documentada

Las rutas actuales se mueven bajo `/api/v1/`; `/api/` queda como alias
durante v0.2 con cabecera `Deprecation`. El contrato vive en
`docs/api/openapi.yaml` y un test (`TestRoutesMatchOpenAPI`, el mismo patrón
que usa trapline) comprueba que la tabla de rutas y el documento coinciden en
las dos direcciones.

Novedades, con su uso propio en pm:

| Función | Ruta | Uso en pm sin consumidor externo |
|---|---|---|
| **Versión y capacidades** | `GET /api/v1/version` → `{version, api: 1, caddy: bool, docker: bool, systemd: bool}` | `pm doctor` la muestra; un cliente decide qué puede pedir |
| **Arriendo de puertos** | `POST /api/v1/leases {owner, count, prefer}` → `{id, ports[]}` · `DELETE /api/v1/leases/{id}` · `GET /api/v1/leases` | `pm lease 3 --owner wt-qa` reserva puertos para un worktree antes de arrancar nada; `pm ls` los muestra como «reservado por» |
| **Arranque por especificación** | `POST /api/v1/run {name, dir, cmd[], env{}, limits{memory, cpu}, log, owner, ports[]}` → `{unit, pid, ports}` | `pm run --name x --mem 512M -- npm run dev` arranca sin registrar; aparece como efímero con dueño |
| **Límites** | campo `limits` en `run` y en `Project` | `pm start api --mem 2G --cpu 150%`; `pm doctor` avisa si no hay cgroups v2 |
| **Compose con nombre** | `compose: {file, project}` en `run` y en `Project` | dos copias del mismo repo (worktrees) sin colisión de contenedores ni volúmenes |
| **Grupos por dueño** | `owner` en `run`, `leases` y `Project`; `POST /api/v1/owners/{owner}/stop` · `DELETE /api/v1/owners/{owner}` (para todo y libera arriendos) | `pm down --owner wt-qa` apaga y limpia todo lo de un worktree de una vez |
| **Adopción por marcador** | ya existe (`PM_PROJECT` + `/api/claim`); se añade `PM_OWNER` | un script que deja procesos vivos los deja con dueño y pm los agrupa |
| **Eventos** | `/api/v1/events` (SSE, ya existe) emite también `lease.*` y `owner.*` | el dashboard refleja arriendos |

Las unidades creadas por `run` se llaman `pm-<name>.service`, como hoy. El
arriendo **no** abre el puerto: lo marca ocupado en la foto de pm para que
nadie más lo reciba, y lo comprueba con un bind de prueba (`net.Listen`), que
hoy no existe y que corrige el caso de un puerto que `ss` no ve.

### Cliente oficial en Go

`github.com/antoniojosev/pm/client`, paquete del mismo módulo (MIT), solo
biblioteca estándar, generado o escrito a mano desde `openapi.yaml`:

```go
c := client.New("http://127.0.0.1:9797")
v, _ := c.Version(ctx)
lease, _ := c.Leases.Create(ctx, client.LeaseRequest{Owner: "wt-qa", Count: 2})
unit, _ := c.Run(ctx, client.RunSpec{Name: "api-qa", Dir: dir, Cmd: cmd, Env: env,
    Limits: client.Limits{Memory: "2G", CPU: "150%"}, Owner: "wt-qa", Ports: lease.Ports})
```

El cliente declara la versión mínima de API que entiende y falla con un error
claro si `GET /api/v1/version` devuelve otra. La CLI de pm **no** lo usa
todavía; lo hará cuando esté estable (misma regla que trapline con su cliente).

### Lo que no cambia

La CLI, el dashboard, las 13 tools MCP (se añaden `pm_run`, `pm_lease`,
`pm_owner_down`) y el modelo de proyectos registrados. Nada bajo `internal/`
se exporta.

## Plan de trabajo

Cada paso deja el repo verde (`make vet test`) y es un commit propio.

| # | Paso | Hecho cuando |
|---|---|---|
| 1 | `/api/v1/` con alias `/api/`; `docs/api/openapi.yaml` con las rutas de hoy; `TestRoutesMatchOpenAPI` | el test falla si se añade una ruta sin documentarla |
| 2 | `GET /api/v1/version` + `pm doctor` lo muestra | — |
| 3 | Límites: `Properties` en el runner (`-p MemoryMax=… -p CPUQuota=…` y en la unidad escrita); campos en `Project`; flags `--mem/--cpu` | un proyecto con `--mem 256M` que lo supera queda `failed` y pm lo reporta |
| 4 | Arriendos: tabla en el registry, `Free(port)` con bind de prueba, rutas, `pm lease`, eventos | dos clientes que piden a la vez nunca reciben el mismo puerto (test con carrera) |
| 5 | `run` por especificación con `owner`, `env`, `limits`, `log`, `ports`; `pm run`; tool `pm_run` | `pm run --name x -- sleep 100` aparece en `pm ls` con dueño y se para con `pm stop x` |
| 6 | Compose con nombre: `-p` y `COMPOSE_PROJECT_NAME` en `run` y en proyectos docker; `docker ps` atribuido por etiqueta `com.docker.compose.project` (ya se lee) | dos `run` del mismo compose con nombres distintos conviven |
| 7 | Dueños: `PM_OWNER`, rutas de `owners`, `pm down --owner`, tool `pm_owner_down`; `DELETE` libera arriendos | apagar un dueño deja `pm ls` sin rastro suyo |
| 8 | `client/` con ejemplo en `example_test.go` y README propio de 30 líneas | el ejemplo compila como test; test de contrato cliente ↔ daemon en memoria |
| 9 | README: «transient service», qué hace `scan` y qué `detector`, sección **«Use pm from your tool»** con el ejemplo del cliente | `grep -ri "transient scope"` vacío |
| 10 | CI: `.github/workflows/ci.yml` (vet, gofmt, `go test -race`, golangci-lint) y `release.yml` con goreleaser (linux amd64/arm64) | badge verde |
| 11 | `docs/adr/` con este ADR; `CHANGELOG.md`; tag **v0.2.0** | `go get github.com/antoniojosev/pm/client@v0.2.0` desde amnesia |

Estimación: 5–6 días de trabajo efectivo.

## Consecuencias

- **Para quien usa pm solo**: tres comandos nuevos (`run`, `lease`, `down
  --owner`), límites por proyecto y una API documentada. Nada se rompe.
- **Para amnesia**: importa `pm/client`, exige `api: 1`, y no lanza ni un
  `systemd-run` por su cuenta.
- **Para el repo**: pasa de herramienta local a servicio con contrato, CI,
  releases y ADRs, con un consumidor nombrado en el README.
- **Guardia**: si en seis meses toda la hoja de ruta de pm viene de amnesia,
  la regla de arriba falló y conviene absorber pm en amnesia. Se revisa en el
  ADR siguiente que toque la API.
