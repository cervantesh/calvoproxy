# P6 — progreso

- **Invariantes 1–7 (+ EOF y `/trace`): en verde.** Nueve tests en
  `cmd/chat_test.go`; las tres puertas pasan.
- **La spec resultó corta en un punto**: no decía qué pasa con el historial cuando
  un turno falla. Si el upstream devuelve error o el transporte cae, el mensaje
  del usuario se **retira** del historial: dejarlo dentro haría que el siguiente
  turno enviase un prompt que ningún modelo llegó a ver, y el REPL mentiría sobre
  lo que mandó. Implementado así; queda registrado aquí porque no estaba escrito.
- **Segundo detalle no especificado**: el buffer de `bufio.Scanner`. El defecto de
  64 KiB trunca en silencio un fichero pegado, que es justo lo que un operador va
  a probar. Subido a 8 MiB en la entrada y 4 MiB en el parseo SSE.
- Pendiente para quien siga: `renderTrace` es la única pieza que conoce el formato
  del header. Si el esquema de P1 sube a `v2`, este es el sitio que hay que tocar.
