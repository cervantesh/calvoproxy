# P3 — progreso

- **Invariantes 1–10 en verde**, más dos de integración por el camino real del
  router. Tres puertas OK.
- **El gate de cobertura cazó lo que la revisión no**: tras enganchar la
  compresión en `dispatchChain`, esa función bajó de 92.9% a 92.1% porque los
  tests unitarios probaban los motores pero nadie ejercitaba la rama nueva por el
  camino real. Probar el motor y probar que está enchufado son dos afirmaciones
  distintas; añadidos un test que comprime de extremo a extremo y su contrario
  con la compresión apagada.
- **Decisión no prevista en la spec**: `safeCompress` con `recover()`. Los cuerpos
  vienen de clientes y acabarán conteniendo de todo; un fallo aquí debe degradar
  a "sin comprimir", nunca a un 500.
- **Sigue apagado por defecto.** Sin `PROXY_COMPRESS_PROFILES` no se toca nada, y
  el camino por defecto tiene su propio test para que no regrese.
- Pendiente antes de encenderlo en real: correr con `PROXY_COMPRESS_DRYRUN=true`
  una temporada y comparar el ahorro medido con la calidad de las respuestas. El
  diseño lo permite; el juicio es humano.
