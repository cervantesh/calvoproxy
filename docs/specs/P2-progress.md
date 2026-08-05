# P2 — progreso

- **Invariantes 1–10 en verde**, más cuatro hermanos que salieron al implementar.
  Las tres puertas pasan.
- **La spec estaba equivocada en dos sitios**, corregidos en el propio documento
  (§7b) antes que en el código:
  1. "Sin límite conocido, headroom es 1" ignoraba que el presupuesto de *cuenta*
     limita a un modelo sin techo propio. El test original fallaba con razón.
  2. La traza contaba las exclusiones por cuota como del breaker, porque
     `ExcludedByBreaker` era una resta derivada. Con el skip duro activo, un
     presupuesto agotado se reportaba como circuito roto. Añadido `q=`.
- **Decisión que queda pendiente de datos reales**: de dónde salen los límites.
  El ledger soporta config, cabeceras y aprendizaje desde 429, pero cuál es la
  fuente primaria depende de si OpenRouter emite `X-RateLimit-*` de forma fiable
  en el free tier. Hasta medirlo, sin `PROXY_QUOTA_LIMITS_JSON` el gate no actúa:
  cuenta, pero no degrada. Eso es deliberado — inventar un techo sería peor.
- El defecto es **blando**: `PROXY_QUOTA_HARD_SKIP` sigue apagado.
