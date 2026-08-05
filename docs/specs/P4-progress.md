# P4 — progreso

- **Invariantes 1–9 en verde**, diez tests en `cmd/setup_test.go`. Tres puertas OK.
- **Bug real encontrado por el test, no por revisión**: `flag.Parse` de Go se
  detiene en el primer argumento posicional, así que `setup codex --apply` parseaba
  cero flags y se comportaba como `--check` — es decir, `--apply` no hacía nada en
  silencio. Resuelto con `splitToolAndFlags`, que separa el nombre de la
  herramienta de los flags en cualquier orden. Sin el invariante 5 esto se habría
  entregado roto.
- **La spec no decía dónde viven los backups en Windows.** `os.UserConfigDir()` es
  `APPDATA` allí y `XDG_CONFIG_HOME` fuera; el test lo pregunta en vez de fijar una
  plataforma.
- Pendiente: Cursor, Cline y Aider como adapters de `Integration`. La interfaz ya
  cubre los tres formatos (YAML solo-lectura, JSON, TOML) que eran el riesgo real.
