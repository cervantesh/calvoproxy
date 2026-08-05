# P6 — `calvoproxy chat`

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md). Consume la traza
de [P1](P1-decision-trace.md).

## 1. Problema

Probar una cadena hoy exige montar Hermes o escribir `curl` con el cuerpo a mano, y `curl` no
descodifica la traza: te deja leyendo `v1;p=coding;s=0.83;a=2;prev=...` a ojo. Hace falta un
cliente propio, de diagnóstico, que hable con el proxy como lo haría un agente y **enseñe la
decisión en cristiano después de cada turno**.

Es además el dogfooding de P1: si la traza no sirve para imprimir "servido por X, saltados Y
(breaker) y Z (cuota)", está mal diseñada — y conviene descubrirlo antes de que Hermes la
parsee.

## 2. Alcance

Es un **cliente**. No importa `internal/router` ni reimplementa nada de la cadena: habla HTTP
con el proxy que ya está corriendo. Sin framework de TUI: `bufio` sobre stdin y códigos ANSI.
Una TUI real serían miles de líneas vendorizadas para una herramienta que compite con `curl`.

```
calvoproxy chat [--profile coding] [--url http://127.0.0.1:8080] [--no-stream]
```

`--url` por defecto sale de `proxyBaseURL()` ([cmd/doctor.go:274](../../cmd/doctor.go)), que ya
respeta el puerto configurado.

## 3. Comportamiento

- Bucle: lee una línea de stdin, la añade al historial como `user`, envía **todo** el
  historial (el upstream es stateless), imprime los deltas según llegan y añade la respuesta
  como `assistant`.
- Streaming por defecto (`stream:true`), que es como lo usan los agentes. `--no-stream` para
  el caso no-streaming.
- Tras cada turno imprime la traza descodificada en una línea.
- Comandos de barra: `/profile <nombre>` cambia de perfil, `/reset` vacía el historial,
  `/trace` alterna el detalle completo (manda `X-Calvoproxy-Trace: full`), `/quit` sale.
  EOF (Ctrl-D) equivale a `/quit`.
- Un error HTTP se imprime con su estatus y su cuerpo, y el REPL **sigue vivo**: un 503 de
  cadena agotada es información, no motivo de cierre.

### 3.1 Render de la traza

`v1;p=coding;s=0.83;a=2;n=4/4/3;prev=gpt-oss-20b:429;brk=1;cmp=off` se imprime como:

```
· coding · nemotron-3-super-120b-a12b · score 0.83 · intento 2/3 · 1 excluido por breaker
  antes falló: gpt-oss-20b (429)
```

Reglas: si el intento es 1 no se menciona (es el caso normal); `brk=` solo si es > 0; la línea
de `antes falló` solo si hay `prev=`; `trunc=1` añade `(traza recortada)`.

## 4. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | Envía a la ruta del perfil elegido, con el historial completo y `stream` según la opción | servidor de prueba que captura ruta y cuerpo |
| 2 | Imprime los deltas de una respuesta SSE en orden y sin los envoltorios `data:` | servidor que emite SSE conocido |
| 3 | La traza se descodifica a texto legible; sin cabecera no se inventa nada | render puro sobre cabeceras de ejemplo, incluida la vacía |
| 4 | Un error HTTP se muestra y el REPL sobrevive al turno | servidor que devuelve 503, seguido de un turno correcto |
| 5 | La respuesta se añade al historial, así que el segundo turno manda tres mensajes | dos turnos contra el mismo servidor |
| 6 | `/profile`, `/reset` y `/quit` hacen lo suyo; `/quit` y EOF terminan con código 0 | guion de entrada con los comandos |
| 7 | `--no-stream` usa la ruta no-streaming y extrae `choices[0].message.content` | servidor que responde JSON no-SSE |

## 5. Fuera de alcance

Historial persistente entre ejecuciones, edición de línea con historial (flechas), y colores
configurables. Es una herramienta de diagnóstico, no un cliente de chat.
