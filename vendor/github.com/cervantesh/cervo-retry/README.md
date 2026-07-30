# cervo-retry

Utilidades compartidas de CervoSoft para clasificar errores HTTP/transporte y calcular esperas de reintento con backoff exponencial.

El paquete ayuda a que proxies, clientes HTTP y workers tomen decisiones consistentes sobre:

- Si una respuesta o error debe reintentarse.
- Si el evento debe contar para abrir un circuit breaker.
- Que codigo HTTP devolver hacia el caller.
- Cuanto esperar entre intentos.

## Instalacion

```bash
go get github.com/cervantesh/cervo-retry
```

Requiere Go `1.25.6` segun `go.mod`.

## Uso rapido

```go
package main

import (
	"fmt"
	"net/http"
	"time"

	cervoretry "github.com/cervantesh/cervo-retry"
)

func main() {
	classification := cervoretry.ClassifyHTTPStatus(http.StatusTooManyRequests, "")

	if classification.Retryable {
		delay := cervoretry.DefaultBackoff().Delay(0)
		fmt.Printf("retry in %s: %s\n", delay, classification.Message)
	}

	custom := cervoretry.BackoffPolicy{
		Initial:    100 * time.Millisecond,
		Max:        2 * time.Second,
		Multiplier: 2,
	}
	fmt.Println(custom.Delay(3))
}
```

## API principal

### `Classification`

Resultado normalizado de una decision de retry:

- `StatusCode`: codigo HTTP recomendado para devolver o propagar.
- `Message`: mensaje resumido para logs o respuestas de error.
- `BreakerEligible`: indica si el fallo debe alimentar un circuit breaker.
- `Retryable`: indica si tiene sentido intentar nuevamente.

### `ClassifyHTTPStatus(statusCode int, responseBody string) Classification`

Clasifica una respuesta HTTP upstream.

| Upstream | Resultado | Retryable | BreakerEligible |
| --- | --- | --- | --- |
| `429 Too Many Requests` | `503 Service Unavailable` | si | si |
| `408 Request Timeout` | `504 Gateway Timeout` | si | si |
| `5xx` | `502 Bad Gateway` | si | si |
| Otros codigos | Mantiene el codigo original | no | no |

Si `responseBody` viene vacio, usa el texto estandar del codigo HTTP.

### `ClassifyTransportError(err error) Classification`

Clasifica errores de transporte antes de recibir una respuesta HTTP.

- `nil`: devuelve `200 OK`, sin retry.
- `context.DeadlineExceeded`: devuelve `504 Gateway Timeout`, retryable.
- `net.Error` con timeout: devuelve `504 Gateway Timeout`, retryable.
- Otros errores de transporte: devuelve `502 Bad Gateway`, retryable.

### `IsRetryableStatus(statusCode int) bool`

Devuelve `true` para codigos que el paquete considera reintentables:

- `429 Too Many Requests`
- `408 Request Timeout`
- Cualquier codigo `5xx`

### `BackoffPolicy`

Define una politica de backoff exponencial.

```go
policy := cervoretry.BackoffPolicy{
	Initial:    250 * time.Millisecond,
	Max:        5 * time.Second,
	Multiplier: 2,
}

delay := policy.Delay(2) // 1s
```

`Delay(attempt)` trata intentos negativos como `0` y aplica valores seguros cuando los campos estan en cero o fuera de rango:

- `Initial`: `250ms`
- `Max`: `5s`
- `Multiplier`: `1` cuando se configura menor a `1`

## Backoff por defecto

```go
policy := cervoretry.DefaultBackoff()
```

Valores:

- `Initial`: `250ms`
- `Max`: `5s`
- `Multiplier`: `2`

Secuencia esperada: `250ms`, `500ms`, `1s`, `2s`, `4s`, `5s`, ...

## Pruebas

```bash
go test ./...
```
