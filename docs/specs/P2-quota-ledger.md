# P2 — Presupuesto de cuota por modelo y por cuenta

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problema

Los límites del free tier se descubren **chocando**: llega un 429, el breaker abre el circuito
y el request ya se ha gastado. El breaker es reactivo por diseño y está bien que lo sea. Lo
que falta es lo predictivo: saber cuánto queda de la ventana y degradar *antes* de agotarla.

## 2. La clave no es `breakerKey`

`breakerKey` es `profile + ":" + model` ([router_breaker.go:189](../../internal/router/router_breaker.go)),
y `allKnownAttempts` genera una entrada **por cada par** perfil-modelo. Para fiabilidad es
correcto: el mismo modelo bajo `coding` y bajo `bulk` recibe cargas distintas y merece
circuitos y scores independientes.

Para cuota es **fatal**. El cupo de OpenRouter es por cuenta y por modelo, no por perfil: con
el mismo slug en varios perfiles —que es el caso hoy en `model-policy.json`— dos contadores
parciales del mismo bolsillo jamás detectarían el agotamiento, cada uno viendo la mitad.

```go
type quotaScope string // "model:<slug>" | "account"
```

El ámbito `account` es obligatorio desde el día uno: es el límite dominante del free tier, y
añadirlo después invalidaría el fichero ya persistido.

## 3. Por qué un ledger separado y no el breaker

Escribir `state.OpenUntil = windowReset` para heredar `filterAvailableAttempts` parece
economía y es un bug:

1. `recordSuccess` ([:180](../../internal/router/router_breaker.go)) y `resolveProbe` ([:160](../../internal/router/router_breaker.go))
   ponen `OpenUntil = time.Time{}` **incondicionalmente**. Un request en vuelo que termine en
   200 borraría en silencio la exclusión por cuota recién puesta.
2. `recordFailure` resetea `ConsecutiveFailures` cuando la ventana expiró: una ventana diaria
   secuestraría la máquina half-open 24 h.
3. `Health()` ([:302](../../internal/router/router_breaker.go)) clasifica todo `OpenUntil`
   futuro como `"open"` y degrada `Status` a `unavailable`: una cuota agotada se reportaría
   como avería.

Ledger propio, con su mutex. **Orden de locks obligatorio: `breakerMu → quotaMu`**, porque
`isModelAvailableLocked` se llama desde `Health()` con `breakerMu` ya tomado. El ledger no
puede llamar a nada que tome `breakerMu`.

## 4. De dónde salen los límites

Por prioridad, y **ninguna inventa un techo**:

1. **Configuración explícita** en `PROXY_QUOTA_LIMITS_JSON`
   (`{"model:openai/gpt-oss-20b:free":{"rpd":50},"account":{"rpd":1000}}`). No se toca
   `model-policy.json`: su forma la fija el paquete vendorizado.
2. **Cabeceras del upstream** `X-RateLimit-Limit` / `-Remaining` / `-Reset` cuando lleguen.
3. **Aprendizaje desde un 429 con `Retry-After`**, que fija `ResetAt` pero **no** un `Limit`:
   un 429 dice "ahora no", no dice cuántas caben.

Sin límite conocido no hay gate: el ledger cuenta pero `headroom` es 1. Fingir un techo sería
peor que no tenerlo.

## 5. Degradación

- **Blanda, por defecto.** `rankAttemptsByScore` ordena por `score × headroom`, con
  `headroom ∈ [0,1]` derivado del porcentaje de ventana consumido. **No toca el score
  persistido**: el score mide fiabilidad, no presupuesto, y contaminarlo envenenaría su decay
  de dos relojes.
- **Dura, solo bajo `PROXY_QUOTA_HARD_SKIP=true`.** Al 100 % de una ventana el modelo se
  excluye, con motivo `quota` en la traza de P1 y `Retry-After` igual al mínimo entre el
  cooldown del breaker y el `ResetAt` de la ventana.

El defecto es blando porque la exclusión dura amplía la superficie de 503 "all cooling"
apoyándose en límites que pueden ser aprendidos y por tanto inexactos.

## 6. Persistencia

Fichero **propio**, `quotas.json` junto a `scores.json`. No se mete en el store de scores: su
caducidad es `ResetAt`, incompatible con `defaultScoreMaxAge`
([router_scoring_store.go:30](../../internal/router/router_scoring_store.go)), que descarta el
fichero entero a las 24 h; y `restoreScores` filtra por `knownBreakerKeys()`, claves que la
cuota no usa. Se comparte el helper de escritura atómica temp+rename, no el fichero.

Al cargar: si `ResetAt` ya pasó, la ventana vuelve a cero — **no se descarta**. Un contador
diario tiene que sobrevivir al reinicio del proxy, porque la ventana del upstream no se
reinicia porque nosotros lo hagamos.

## 7. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | La cuota se indexa por modelo desnudo: el mismo slug en dos perfiles comparte contador | consumir bajo dos perfiles; el contador es la suma |
| 2 | El ámbito `account` cuenta todo request, sea el modelo que sea | dos modelos distintos; `account` los suma |
| 3 | Al pasar `ResetAt`, la ventana vuelve a cero en vez de descartarse | ventana vencida; `Used` es 0 y el límite se conserva |
| 4 | La cuota persiste y se restaura; una ventana ya vencida carga a cero | guardar, recargar |
| 5 | La degradación blanda reordena pero **no** altera el score persistido | score idéntico antes y después |
| 6 | Sin límite conocido, `headroom` es 1 y no hay gate | modelo sin límite configurado |
| 7 | La exclusión dura solo ocurre con `PROXY_QUOTA_HARD_SKIP` | mismo estado, las dos configuraciones |
| 8 | Un 429 sigue abriendo el breaker: la cuota no lo sustituye | 429; se afirma `ConsecutiveFailures` |
| 9 | `Health()` con `breakerMu` tomado consultando el ledger no bloquea | carga concurrente bajo `-race` |
| 10 | Un 429 con `Retry-After` fija `ResetAt` pero no inventa `Limit` | tras aprender, `Limit` sigue a 0 |

## 7b. Correcciones a esta spec durante la implementación

1. **"Sin límite conocido, headroom es 1" era incompleto.** `headroom` es el mínimo sobre
   *todas* las ventanas que aplican, y el presupuesto de **cuenta** limita legítimamente a un
   modelo que no tiene techo propio. Aislar el caso del invariante 6 exige un ledger sin
   ningún límite configurado, no solo sin límite de modelo. Añadido un invariante hermano que
   fija lo contrario: con `account` configurado y el modelo sin techo, el headroom lo marca la
   cuenta.

2. **La traza contaba las exclusiones por cuota como si fueran del breaker.**
   `ExcludedByBreaker` se derivaba por resta (`afterCaps - eligible`), así que con el skip duro
   activo un presupuesto agotado se reportaba como circuito abierto — justo la ambigüedad que
   P1 existe para eliminar. Añadido `ExcludedByQuota` y un campo `q=` al header, y la resta del
   breaker ahora lo descuenta. Esto **modifica el contrato de P1**, de forma aditiva: `q=` es
   un campo nuevo y opcional, dentro de `v1`.

## 8. Fuera de alcance

Coordinación entre varias réplicas (el ledger es local, como los scores) y cuotas por token
en vez de por request: el free tier limita peticiones, y contar tokens exigiría fiarse del
`usage` de cada respuesta, que no siempre viene.
