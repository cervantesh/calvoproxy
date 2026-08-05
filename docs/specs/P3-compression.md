# P3 — Guardia de tamaño de resultados de herramienta

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

> **Esta spec cambió de alcance después de implementarse.** La versión original
> describía dos motores de compresión dentro del proxy. Estaban en la capa
> equivocada y se movieron a `github.com/cervantesh/cervo-compress`. Lo que queda
> aquí es lo único que sí es competencia de un proxy. El razonamiento completo
> está en §2, porque el error es más instructivo que el resultado.

## 1. Qué hace

Recorta un resultado de herramienta que supere `PROXY_TOOL_RESULT_LIMIT`,
conservando **los dos extremos** con un marcador explícito en medio. **Apagado
por defecto**: sin esa variable el proxy reenvía exactamente lo que recibió.

Es de la misma familia que `PROXY_MAX_RESPONSE_BYTES`: una declaración de qué
está dispuesto a transportar este proxy, no un juicio sobre lo que el modelo
necesita.

## 2. Por qué los motores se fueron

Decidir qué puede tirarse de una conversación exige **conocer esa conversación**:
qué resultado de herramienta sigue importando, qué está haciendo el usuario, si
un bloque puede recuperarse cuando el modelo lo pida. El proxy ve una foto
stateless y no sabe nada de eso. Hermes y los agentes sí — son dueños de la
conversación.

Un detalle de OmniRoute lo confirma: su motor CCR, el único que **quita**
contenido de verdad, solo inyecta su protocolo de recuperación si el llamante
expone la herramienta `omniroute_ccr_retrieve`. Es decir, ni siquiera ellos
quitan contexto sin un contrato con el cliente.

`dedup` se fue entero. `toolcap` se quedó, reencuadrado: ya no es "comprimir",
es "no transportar medio megabyte en un mensaje".

## 3. Reglas

- **Solo mensajes `role: "tool"`.** Un mensaje de usuario es lo que se pidió.
- **Nunca contenido que sea JSON válido.** Recortarlo produce JSON inválido, y
  un resultado corrupto es peor que uno largo.
- **Nunca contenido estructurado** (arrays de bloques: imágenes, `tool_result`
  del dialecto Anthropic). No hay forma genérica segura de recortarlo.
- **Suelo de 512 bytes.** Por debajo, el marcador sería casi todo lo que
  sobrevive.
- **Si el marcador cuesta más que el recorte**, no se toca.
- **Ante cualquier pánico**, se reenvía el cuerpo original y se registra un
  aviso. Un fallo aquí degrada a "sin recortar", nunca a un 500.

## 4. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | Apagado por defecto: el cuerpo sale idéntico | comparación byte a byte |
| 2 | Nunca muta el mapa de entrada | copia guardada y comparada |
| 3 | No toca JSON válido | resultado JSON largo |
| 4 | Conserva principio y final, y marca el recorte | contenido largo no-JSON |
| 5 | No toca mensajes que no sean `role: tool` | mensaje de usuario largo |
| 6 | Un límite absurdo se ajusta al suelo | `PROXY_TOOL_RESULT_LIMIT=1` |
| 7 | Formas raras no revientan | mensajes nulos, tipos mezclados |
| 8 | El recorte llega a la traza como `cmp=` | header tras recortar |
| 9 | Funciona por el camino real del router, y apagado no altera nada | dos tests de integración |

## 5. Fuera de alcance

Todo lo que sea gestión de contexto. Vive en
[`cervo-compress`](https://github.com/cervantesh/cervo-compress), como librería,
para que la use quien es dueño de la conversación.
