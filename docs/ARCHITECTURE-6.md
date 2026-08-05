# CalvoProxy — Arquitectura de alto nivel para las 6 capacidades

Síntesis de un panel de 3 arquitectos independientes (2 rondas: diseño ciego + arbitraje
cruzado), con las afirmaciones factuales verificadas contra el código.

Nomenclatura: **P1** header de decisión · **P2** cuotas · **P3** compresión ·
**P4** `setup-<tool>` · **P5** dashboard · **P6** `chat`.

---

## 0. La forma del sistema en una frase

Una **única estructura de traza por request**, poseída por la goroutine del request y completa
antes del primer byte de respuesta, es la fontanería de la que cuelgan P1, P5, P6 y la
*medición* de P2 y P3. La **cuota (P2) no vive ahí**: es estado durable compartido entre
requests, hermano del scoring, con su propio fichero y su propia clave. P4 es tooling y no
toca el router.

```
dispatchChain                     ← crea la traza, la mete en el ctx
  ├─ applyCapabilityFilter        ← anota exclusiones (capability)
  ├─ filterAvailableAttempts      ← anota exclusiones (breaker) + consulta quotaLedger
  ├─ rankAttemptsByScore          ← anota orden + factor de headroom de cuota (P2 soft)
  ├─ [P3] compresión, UNA vez     ← anota compressionStats
  └─ executeFallbacks
       └─ Execute (bucle)         ← anota cada attemptError
            └─ ExecuteAttempt
                 └─ executeAttempt
                      ├─ 429 → parseRetryAfter → recordFailure   ← P2 ingiere consumo
                      └─ setServedModelHeaders                   ← P1 materializa aquí
```

---

## 1. Decisiones cerradas

### D1 — La traza viaja en el `context.Context`. Verificado.

`setServedModelHeaders` se invoca en `router_upstream.go:209` (streaming) y `:259`
(no-streaming), ambas **dentro de** `executeAttempt` (que abarca `:16`–`:263`).
`AttemptExecutor.ExecuteAttempt` (`router_types.go:157`) **no recibe `FallbackExecution`**.
Por tanto un campo en ese struct no alcanza el punto donde la traza se materializa, ni el
`Retry-After` del 429 (`:138`), ni la latencia de primer token (`:204`), ni el desenlace del
stream (`:213-229`). El `ctx` sí llega a todos.

Queda desmentida la razón original para preferir el `ctx` ("añadir un campo rompe las firmas"):
**es falsa** — el struct va por valor, las firmas mencionan el tipo y no sus campos, y los
nueve literales del repo (`router_fallback_test.go:42,73,103`;
`router_chain_failure_test.go:108,135,159,174,189`; `router.go:347`) son con claves. Pero la
conclusión sobrevive por un motivo mejor: **alcance**, no compatibilidad.

**Decisión:** puntero en el `ctx` con clave tipada, y *nada* de duplicarlo también como campo
de `FallbackExecution` — dos fuentes del mismo puntero es un olor, no legibilidad. Un accesor
`traceFrom(ctx)` que devuelve `nil` fuera de banda, con todos los `record*` como no-op sobre
`nil` (el patrón que ya usa `s.capabilities != nil`, `router.go:618`).

**Invariantes a escribir en comentario y proteger con `-race`:** un solo escritor (la goroutine
del request); `streamCopy` (`router_stream.go:97`) y `awaitFirstStreamEvent` (`:236`) no la
tocan; al ring de P5 se copia una versión compactada al cerrar, nunca se publica el puntero.

### D2 — Header corto por defecto, JSON completo bajo opt-in, `/decisions/{id}` como tercera vía.

La forma corta versionada (`v1;p=coding;s=0.83;a=2;prev=…;caps=tools;cmp=-3.1k`, tope duro
512 B) es el contrato estable. El JSON completo solo si el cliente manda
`X-Calvoproxy-Trace: full`. Y `GET /decisions/{id}` sobre el ring, con el mismo gate `admin`
que `/health`.

El motivo real no es el tamaño (en un binario loopback de un salto, 1–2 KiB no rompe nada, y
gRPC los hereda gratis porque `cmd/grpc.go:100` copia `recorder.Header()`): es que la traza
lleva `LastFailureReason` de otros modelos, que es cuerpo de error upstream truncado
(`truncateReason`, `router_breaker.go:559`). Emitir eso por defecto en toda respuesta es
filtración, no verbosidad. Sanitizar siempre.

`cmp=` debe estar **siempre presente**, incluso como `cmp=off`, para que "no se comprimió" sea
distinguible de "el campo no existe".

### D3 — `quotas.json` separado. No extender `scores.json`.

`LoadScores` descarta el fichero entero si la versión no coincide
(`router_scoring_store.go:232`) o si supera `maxAge` (`:237`), y `restoreScores` filtra por
`knownBreakerKeys()` (`:133`). Meter cuotas dentro exige **tres excepciones a esas tres
reglas** en el mismo loader, más un cuarto problema: `snapshotScores` toma `breakerMu`
(`:94`), lo que acoplaría el flush de cuotas al lock del breaker. Y el bump de versión tira
los scores de todo el parque, con v1 y v2 coexistiendo durante el rollout.

**Lo que sí se comparte:** extraer el helper de escritura atómica temp+rename de
`writeScoreFile` (`:175`, 0600/0700) y el patrón dirty-flag + flusher de 30 s a un
`statestore` común.

**Ciclo de vida propio:** la caducidad de una cuota es su `ResetAt`, no `defaultScoreMaxAge`.
Al cargar: si `ResetAt` ya pasó, ventana a cero; si no, se restaura `Used`.

### D4 — Cuota indexada por `model:<slug>` + un ámbito `account`. Unánime tras arbitraje.

`breakerKey` es `profile + ":" + model` (`router_breaker.go:189`), y eso es **correcto para
fiabilidad** — el mismo slug en `coding` y en `agent` trae distinto perfil, distinto
`decision.Timeout` (`router.go:344`) y distinto cuerpo, así que merece circuitos y scores
independientes.

Es **fatal para cuota**: el cupo de OpenRouter es por cuenta+modelo. Con el mismo slug en
varios perfiles de `model-policy.json` (que es exactamente el caso hoy), `coding:x` y
`agent:x` llevarían contadores parciales del mismo bolsillo y el gate no se dispararía nunca a
tiempo. El ámbito `account` es obligatorio desde el día uno — es el límite dominante del free
tier y añadirlo después invalida el fichero persistido.

### D5 — `quotaLedger` con estado propio; se reutilizan los *choke points*, no el estado.

Escribir `state.OpenUntil = windowReset` para heredar el filtrado del breaker está desmentido
por tres sitios del código:

1. `recordSuccess` (`router_breaker.go:180`) y `resolveProbe` (`:160`) ponen
   `OpenUntil = time.Time{}` incondicionalmente. Un request **en vuelo** que termine en 200
   borraría en silencio la exclusión por cuota recién puesta.
2. `recordFailure` resetea `ConsecutiveFailures` y `OpenUntil` cuando la ventana expiró
   (`:111-114`): una ventana diaria secuestraría la máquina half-open 24 h.
3. `Health()` clasifica todo `OpenUntil` futuro como `"open"` y degrada `Status` (`:302` y ss.):
   una cuota agotada se reportaría como circuito roto — justo el diagnóstico confuso que P1
   existe para eliminar.

**Diseño:** ledger con su propio mutex, consultado desde `isModelAvailableLocked` (`:37`) y
`retryAfterForAttempts` (`:223`) *además* del breaker — así se hereda igualmente el
`Retry-After` del 503 (`router.go:332`), tomando el mínimo de ambos.
**Orden de locks obligatorio: `breakerMu → quotaMu`**, porque `isModelAvailableLocked` se llama
desde `Health()` con `breakerMu` ya tomado (`:340`); el ledger no puede llamar a nada que tome
`breakerMu`.

**Degradación blanda por defecto:** factor de headroom ∈ [0,1] aplicado en
`rankAttemptsByScore` (`router_scoring.go:230`) **sin tocar el score persistido** — el score
mide fiabilidad, no presupuesto, y contaminarlo envenena su decay de dos relojes.
Exclusión dura solo bajo `PROXY_QUOTA_HARD_SKIP`.

**El breaker sigue siendo el backstop reactivo.** Matiz factual: el 429 es neutral solo en el
breaker de *host* (`:509`), y el propio comentario dice que lo es *porque el breaker de modelo
sí lo cuenta* — `executeAttempt:135-139` penaliza duro y llama a `recordFailure` con
`parseRetryAfter`. La cuota es predictiva; el breaker, reactivo. No fusionar.

### D6 — Dos motores de compresión. Fuera el dedup de sesión y la poda de prosa.

- **Tool-result truncate** — cola de N bytes con cabecera `[truncado: X bytes]`, respetando
  código y JSON byte a byte. Es el que más rinde en cargas de agente.
- **Dedup intra-request** — copias literales del mismo bloque *dentro del historial que ya se
  envía* se sustituyen por una referencia al bloque que sí viaja. Determinista por hash,
  autocontenido, sin estado ni persistencia.

**Dedup de sesión cross-turn: descartado.** Contra un upstream stateless, no reenviar el
contexto no lo comprime — hace que el modelo deje de verlo. Eso es amnesia. Un LRU con hash de
prefijo tampoco lo salva: el problema no es recordar el prefijo, es que hay que reenviarlo
igual.

**Poda semántica de prosa: descartada en v1.** "Semántico + determinista + sin ML" es una
contradicción práctica: preservar código "byte a byte" mientras se poda texto exige delimitar
código en Markdown arbitrario, y un fence mal cerrado por el modelo convierte la poda en
corrupción. Sobrevive solo su versión no semántica (colapsar blancos y bloques idénticos), que
ya está cubierta por el dedup.

**Enganche:** una sola pasada en `dispatchChain` antes de `executeFallbacks` — nunca dentro
del bucle, que ya re-serializa por intento (`router_fallback.go:108`).
**Obligatorio:** devolver un mapa nuevo, porque `execution.RequestBody["model"] = attempt.Model`
(`router_fallback.go:107`) muta el mapa compartido. Opt-in por perfil en `model-policy.json`,
modo `dry-run` (calcula el ahorro, no aplica) y kill-switch por env. Si un motor falla o el
ahorro queda por debajo del umbral, se reenvía el cuerpo original intacto.

### D7 — Orden: **P1 → P6 → (P2 ∥ P4) → P5 → P3**. Unánime tras arbitraje.

- **P1 primero y solo.** Es el único cambio que toca el hot path; se estabiliza bajo `-race` y
  el gate de cobertura antes de apilar nada.
- **P6 inmediatamente después.** ~150 líneas, y es el único consumidor que ejerce SSE +
  cabeceras + fallback a mano. Si la traza no sirve para imprimir "servido por X, saltados Y
  (breaker), Z (cuota)", está mal diseñada — y eso hay que descubrirlo antes de que Hermes la
  parsee, no después.
- **P2 ∥ P4.** No comparten una línea. P2 es el de mayor valor operativo; P4 es mecánico una
  vez extraído el paquete.
- **P5** con Health + Counters + ring; gana columnas conforme llegan P2 y P3.
- **P3 el último.** Es el único que muta requests y el único con degradación silenciosa:
  necesita la traza (auditar qué se comprimió) y el dashboard (ver el ahorro y la regresión)
  ya operativos.

### D8 — P4: la interfaz primero, tres integraciones, `Apply` escribe con backup.

Interfaz `Integration` con `Detect / Current / Apply / Verify / Revert`, extraída de
`cmd/doctor.go` junto con los helpers YAML line-wise (`yamlScalar:102`, `yamlBlock:147`,
`yamlListEntries:221`), `checkResult:50` y `checkRoundTrip:359` — esta última es el "verificar
que tomó efecto" y se reutiliza tal cual. `doctor` pasa a iterar el registro de integraciones.

Corte inicial: **Hermes + Claude Code + Codex**. Cursor/Cline/Aider después, como adapters de
la misma interfaz — el valor está en validar el contrato, no en cubrir catálogo.

Matiz sobre quién escribe: `doctor` hoy no escribe nada (no hay una sola llamada a
`os.WriteFile`/`os.Create`/`os.OpenFile` en el fichero), y su inspección YAML es una heurística
line-wise. **Una heurística que lee no debe escribir.** Así que `Apply` escribe de verdad, con
backup con timestamp y `--revert`, donde hay parser stdlib fiable (JSON de Claude Code, TOML de
Codex); para Hermes/YAML se queda en imprimir el bloque y verificar el round-trip. Regla dura
en todos los casos: bloque delimitado por marcadores, **nunca round-trip de parser** — destruye
los comentarios del usuario.

### D5b — Dashboard y chat, en una línea cada uno

**P5:** `embed.FS` + HTML/JS vanilla bajo el gate `admin` existente (`cmd/main.go:119`),
polling 2 s, sin WebSockets. No calcula nada: todo agregado que muestre debe existir antes como
snapshot del router. Sin series históricas — para eso ya está `/metrics`.

**P6:** `cmd/chat.go`, REPL stdlib contra el propio `/v1/{perfil}/chat/completions`, sin
framework TUI. Es un cliente: no toca `internal/router`.

---

## 2. Lo que decide el humano, no la arquitectura

**(a) De dónde salen los límites de P2 — el más importante.** Si OpenRouter no emite
`X-RateLimit-*` de forma fiable en el free tier, quedan config manual (que nadie mantiene) o
aprendizaje desde el 429 — que aprende justo el evento que P2 existe para evitar, y que además
puede aprender un techo falso: un 429 puede venir del límite por minuto y no del diario, y
`parseRetryAfter` no los distingue. El diseño soporta las tres fuentes, pero **cuál es la
primaria determina si P2 previene de verdad o solo etiqueta mejor lo que ya pasa.** Esto se
resuelve con una tarde midiendo contra la cuenta real, no con una decisión de diseño.

**(b) Soft vs hard por defecto en la exclusión por cuota.** Excluir al 100 % de ventana amplía
la superficie de "all models cooling down" (más 503 con `Retry-After`) frente a dejar el modelo
al final del ranking y comerse el 429 real. ¿Un 503 predictivo honesto, o quemar un intento en
un 429 probable? Propuesta compatible con los tres: soft por defecto, hard tras N
confirmaciones o vía env.

**(c) La forma del contrato de P1 hacia Hermes.** ¿La forma corta es el único contrato estable
y el detalle vive solo tras `admin`, o Hermes debe poder pedir el JSON completo en el path
caliente de cada completion? Es decisión de API hacia el consumidor, no de fontanería interna.

---

## 3. Riesgos irreversibles

| Riesgo | Mitigación |
|---|---|
| El esquema de P1 es API pública en cuanto Hermes lo parsee | Versión en el primer commit (`v1;…`), campos solo-añadir, forma corta estable / `full` evolutivo |
| P3 puede degradar respuestas **en silencio** | Opt-in por perfil, `dry-run`, umbral mínimo, `cmp=` siempre presente, cuerpo original intacto ante cualquier error |
| El ámbito de la cuota (D4) queda grabado en el fichero persistido | `model` + `account` desde el día uno |
| P4 puede destruir el `settings.json` o el `config.toml` del usuario | Backup con timestamp + `--revert`, marcadores, nunca round-trip de parser |
| Persistir el ring a disco (tentador para P5 tras reinicio) | **Prohibido**: son cuerpos de conversación. La serie durable es `/metrics` |
| El ledger sin locks se rompería con un fan-out especulativo entre modelos | El invariante de un solo escritor va documentado y cubierto por `-race` |
