// Package fnconfig validates the per-invocation runtime settings that
// POST /executions applies to docker run: timeout, memory, CPU, handler
// and environment variables.
package fnconfig

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	TimeoutMin = 1
	TimeoutMax = 300
	MemoryMin  = 64
	MemoryMax  = 2048

	// CPU is expressed in milli-CPUs so bounds checks do not depend on
	// binary float noise. 100 = 0.1 CPU, 4000 = 4 CPUs.
	CPUMinMilli = 100
	CPUMaxMilli = 4000

	EnvMaxCount = 32
	EnvNameMax  = 128
	EnvValueMax = 4096
	EnvTotalMax = 16 * 1024
	HandlerMax  = 128

	DefaultTimeoutSec = 5
	DefaultMemoryMB   = 128
	DefaultHandler    = "handler"
)

// handlerRE accepts "handler", "index.handler" and "src/app.main".
// The optional suffix after the last dot is the export name. A spec with
// no dot is the export on index.js, which is what callers already send.
var handlerRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*(?:\.[A-Za-z_][A-Za-z0-9_]*)?$`)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FieldError is a rejected request field. Message is safe to return to the
// client and to write in logs: it never includes an environment value.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Message }

func fieldErr(field, message string) error {
	return &FieldError{Field: field, Message: message}
}

// Defaults are the values used when the request omits a field.
// CPUsMilli 0 means "do not pass --cpus".
type Defaults struct {
	TimeoutSec int
	MemoryMB   int
	Handler    string
	CPUsMilli  int
}

// Builtin is the compiled-in default, used when the environment does not
// override it or the override is outside the accepted bounds.
func Builtin() Defaults {
	return Defaults{
		TimeoutSec: DefaultTimeoutSec,
		MemoryMB:   DefaultMemoryMB,
		Handler:    DefaultHandler,
	}
}

func (d Defaults) normalized() Defaults {
	b := Builtin()
	if d.TimeoutSec == 0 {
		d.TimeoutSec = b.TimeoutSec
	}
	if d.MemoryMB == 0 {
		d.MemoryMB = b.MemoryMB
	}
	if strings.TrimSpace(d.Handler) == "" {
		d.Handler = b.Handler
	}
	return d
}

// CPUsLabel is a log-safe description of the default CPU limit.
func (d Defaults) CPUsLabel() string {
	if d.CPUsMilli <= 0 {
		return "sin límite"
	}
	return FormatMilli(d.CPUsMilli)
}

// LoadDefaults reads the compute-plane environment.
//
//	NIMBUS_TIMEOUT_SEC   default 5, bounds 1..300
//	NIMBUS_MEMORY_MB      default 128, bounds 64..2048
//	NIMBUS_MEMORY         accepted as "<n>m" when NIMBUS_MEMORY_MB is unset
//	NIMBUS_HANDLER        default "handler"
//	NIMBUS_CPUS           unset means no --cpus; otherwise 0.1..4
//
// A value outside the bounds is ignored and the built-in default is kept.
func LoadDefaults() Defaults {
	d := Builtin()
	if n, ok := envInt("NIMBUS_TIMEOUT_SEC"); ok {
		if n >= TimeoutMin && n <= TimeoutMax {
			d.TimeoutSec = n
		} else {
			log.Printf("[fnconfig] NIMBUS_TIMEOUT_SEC fuera de rango, se usa %d", d.TimeoutSec)
		}
	}
	if n, ok := envMemory(); ok {
		d.MemoryMB = n
	}
	if raw := strings.TrimSpace(os.Getenv("NIMBUS_HANDLER")); raw != "" {
		if _, _, err := SplitHandler(raw); err != nil {
			log.Printf("[fnconfig] NIMBUS_HANDLER inválido, se usa %q", d.Handler)
		} else {
			d.Handler = raw
		}
	}
	if raw := strings.TrimSpace(os.Getenv("NIMBUS_CPUS")); raw != "" {
		milli, err := parseCPUMillis(raw)
		if err != nil {
			log.Printf("[fnconfig] NIMBUS_CPUS inválido, no se aplica límite de CPU")
		} else {
			d.CPUsMilli = milli
		}
	}
	return d
}

func envInt(key string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("[fnconfig] %s no es un entero, se ignora", key)
		return 0, false
	}
	return n, true
}

func envMemory() (int, bool) {
	if raw := strings.TrimSpace(os.Getenv("NIMBUS_MEMORY_MB")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < MemoryMin || n > MemoryMax {
			log.Printf("[fnconfig] NIMBUS_MEMORY_MB inválido, se usa %d", DefaultMemoryMB)
			return 0, false
		}
		return n, true
	}
	raw := strings.TrimSpace(os.Getenv("NIMBUS_MEMORY"))
	if raw == "" {
		return 0, false
	}
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "m"), "M")
	n, err := strconv.Atoi(raw)
	if err != nil || n < MemoryMin || n > MemoryMax {
		log.Printf("[fnconfig] NIMBUS_MEMORY inválido, se usa %d", DefaultMemoryMB)
		return 0, false
	}
	return n, true
}

// Raw is the optional JSON from POST /executions. Empty or null means the
// field was omitted.
type Raw struct {
	TimeoutSec json.RawMessage
	MemoryMB   json.RawMessage
	CPUs       json.RawMessage
	Handler    json.RawMessage
	EnvVars    json.RawMessage
}

// EnvVar is one user variable. Value must not be written to logs.
type EnvVar struct {
	Name  string
	Value string
}

// Resolved is a checked configuration ready for docker run.
type Resolved struct {
	TimeoutSec    int
	MemoryMB      int
	CPUsMilli     int
	CPUsFlag      string
	Handler       string
	HandlerFile   string
	HandlerExport string
	Env           []EnvVar
	// Runtime is nodejs20, python3.12 or go1.x. Empty means nodejs20.
	// It is not a docker limit: the handler validates it and the executor
	// picks the image. Complete leaves it untouched.
	Runtime string
}

// Resolve applies defaults and bounds to one request.
// Zero and omitted numbers use defaults (the previous contract treated 0
// as "not set"). Any other out-of-range number is rejected.
func Resolve(raw Raw, defaults Defaults) (Resolved, error) {
	d := defaults.normalized()
	out := Resolved{
		TimeoutSec: d.TimeoutSec,
		MemoryMB:   d.MemoryMB,
		CPUsMilli:  d.CPUsMilli,
		Handler:    d.Handler,
	}

	if n, set, err := parseOptionalInt(raw.TimeoutSec, "timeoutSec", TimeoutMin, TimeoutMax); err != nil {
		return Resolved{}, err
	} else if set {
		out.TimeoutSec = n
	}
	if n, set, err := parseOptionalInt(raw.MemoryMB, "memoryMb", MemoryMin, MemoryMax); err != nil {
		return Resolved{}, err
	} else if set {
		out.MemoryMB = n
	}
	if milli, set, err := parseOptionalCPU(raw.CPUs); err != nil {
		return Resolved{}, err
	} else if set {
		out.CPUsMilli = milli
	}
	if handler, set, err := parseOptionalHandler(raw.Handler); err != nil {
		return Resolved{}, err
	} else if set {
		out.Handler = handler
	}
	env, err := parseEnv(raw.EnvVars)
	if err != nil {
		return Resolved{}, err
	}
	out.Env = env
	return out.complete()
}

// Complete fills a zero TimeoutSec, MemoryMB or Handler and checks bounds.
// The executor uses it as a backstop when a caller did not go through Resolve.
func (c Resolved) Complete() (Resolved, error) {
	return c.complete()
}

func (c Resolved) complete() (Resolved, error) {
	d := Builtin()
	if c.TimeoutSec == 0 {
		c.TimeoutSec = d.TimeoutSec
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = d.MemoryMB
	}
	if strings.TrimSpace(c.Handler) == "" {
		c.Handler = d.Handler
	}
	file, exportName, err := SplitHandler(c.Handler)
	if err != nil {
		return Resolved{}, err
	}
	c.HandlerFile = file
	c.HandlerExport = exportName
	if c.CPUsMilli == 0 {
		c.CPUsFlag = ""
	} else {
		if c.CPUsMilli < CPUMinMilli || c.CPUsMilli > CPUMaxMilli {
			return Resolved{}, fieldErr("cpus", "cpus debe ser un número entre 0.1 y 4")
		}
		c.CPUsFlag = FormatMilli(c.CPUsMilli)
	}
	if c.TimeoutSec < TimeoutMin || c.TimeoutSec > TimeoutMax {
		return Resolved{}, fieldErr("timeoutSec", "timeoutSec debe ser un entero entre 1 y 300")
	}
	if c.MemoryMB < MemoryMin || c.MemoryMB > MemoryMax {
		return Resolved{}, fieldErr("memoryMb", "memoryMb debe ser un entero entre 64 y 2048")
	}
	if err := validateEnvList(c.Env); err != nil {
		return Resolved{}, err
	}
	return c, nil
}

// SplitHandler maps a handler spec to a file (no extension) and an export.
//
//	"handler"       → index, handler
//	"main"          → index, main
//	"index.handler" → index, handler
//	"src/app.main"  → src/app, main
//	"src/app"       → src/app, handler
func SplitHandler(spec string) (file, exportName string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || len(spec) > HandlerMax || !utf8.ValidString(spec) || !handlerRE.MatchString(spec) {
		return "", "", fieldErr("handler", "handler debe tener la forma export o archivo.export (por ejemplo handler, index.handler o src/app.main)")
	}
	slash := strings.LastIndex(spec, "/")
	dot := strings.LastIndex(spec, ".")
	if dot > slash {
		return spec[:dot], spec[dot+1:], nil
	}
	if slash >= 0 {
		return spec, DefaultHandler, nil
	}
	return "index", spec, nil
}

func parseOptionalInt(raw json.RawMessage, field string, min, max int) (int, bool, error) {
	text, ok := present(raw)
	if !ok {
		return 0, false, nil
	}
	if text == "" || text[0] == '"' {
		return 0, false, fieldErr(field, fmt.Sprintf("%s debe ser un entero entre %d y %d", field, min, max))
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fieldErr(field, fmt.Sprintf("%s debe ser un entero entre %d y %d", field, min, max))
	}
	if f == 0 {
		return 0, false, nil
	}
	if f < 0 || f != math.Trunc(f) {
		return 0, false, fieldErr(field, fmt.Sprintf("%s debe ser un entero entre %d y %d", field, min, max))
	}
	n := int(f)
	if n < min || n > max {
		return 0, false, fieldErr(field, fmt.Sprintf("%s debe ser un entero entre %d y %d", field, min, max))
	}
	return n, true, nil
}

func parseOptionalCPU(raw json.RawMessage) (int, bool, error) {
	text, ok := present(raw)
	if !ok {
		return 0, false, nil
	}
	if text == "" || text[0] == '"' {
		return 0, false, fieldErr("cpus", "cpus debe ser un número entre 0.1 y 4")
	}
	milli, err := parseCPUMillis(text)
	if err != nil {
		return 0, false, err
	}
	if milli == 0 {
		return 0, false, nil
	}
	return milli, true, nil
}

func parseCPUMillis(text string) (int, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fieldErr("cpus", "cpus debe ser un número entre 0.1 y 4")
	}
	if f == 0 {
		return 0, nil
	}
	milli := int(math.Round(f * 1000))
	if milli < CPUMinMilli || milli > CPUMaxMilli {
		return 0, fieldErr("cpus", "cpus debe ser un número entre 0.1 y 4")
	}
	return milli, nil
}

func parseOptionalHandler(raw json.RawMessage) (string, bool, error) {
	if _, ok := present(raw); !ok {
		return "", false, nil
	}
	var handler string
	if err := json.Unmarshal(raw, &handler); err != nil {
		return "", false, fieldErr("handler", "handler debe ser un string")
	}
	handler = strings.TrimSpace(handler)
	if handler == "" {
		return "", false, nil
	}
	if _, _, err := SplitHandler(handler); err != nil {
		return "", false, err
	}
	return handler, true, nil
}

func parseEnv(raw json.RawMessage) ([]EnvVar, error) {
	text, ok := present(raw)
	if !ok {
		return nil, nil
	}
	if text == "" || text[0] != '{' {
		return nil, fieldErr("envVars", "envVars debe ser un objeto de strings")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fieldErr("envVars", "envVars debe ser un objeto de strings")
	}
	if len(obj) > EnvMaxCount {
		return nil, fieldErr("envVars", fmt.Sprintf("envVars admite como máximo %d variables", EnvMaxCount))
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]EnvVar, 0, len(names))
	for _, name := range names {
		if err := validateEnvName(name); err != nil {
			return nil, err
		}
		var value string
		if err := json.Unmarshal(obj[name], &value); err != nil {
			return nil, fieldErr("envVars", fmt.Sprintf("envVars.%s debe ser un string", displayName(name)))
		}
		out = append(out, EnvVar{Name: name, Value: value})
	}
	if err := validateEnvList(out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateEnvList(env []EnvVar) error {
	if len(env) > EnvMaxCount {
		return fieldErr("envVars", fmt.Sprintf("envVars admite como máximo %d variables", EnvMaxCount))
	}
	total := 0
	seen := make(map[string]struct{}, len(env))
	for _, ev := range env {
		if err := validateEnvName(ev.Name); err != nil {
			return err
		}
		if _, ok := seen[ev.Name]; ok {
			return fieldErr("envVars", fmt.Sprintf("la variable %s está repetida", displayName(ev.Name)))
		}
		seen[ev.Name] = struct{}{}
		if strings.ContainsAny(ev.Value, "\r\n\x00") {
			return fieldErr("envVars", fmt.Sprintf("el valor de %s no puede contener saltos de línea", displayName(ev.Name)))
		}
		if len(ev.Value) > EnvValueMax {
			return fieldErr("envVars", fmt.Sprintf("el valor de %s supera %d bytes", displayName(ev.Name), EnvValueMax))
		}
		total += len(ev.Name) + len(ev.Value)
		if total > EnvTotalMax {
			return fieldErr("envVars", fmt.Sprintf("envVars supera el tamaño máximo de %d bytes", EnvTotalMax))
		}
	}
	return nil
}

func validateEnvName(name string) error {
	if name == "" || len(name) > EnvNameMax || !envNameRE.MatchString(name) {
		return fieldErr("envVars", fmt.Sprintf("el nombre de variable %q no es válido", displayName(name)))
	}
	if strings.HasPrefix(strings.ToUpper(name), "NIMBUS_") {
		return fieldErr("envVars", fmt.Sprintf("la variable %s está reservada y no se puede redefinir", displayName(name)))
	}
	return nil
}

func present(raw json.RawMessage) (string, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return "", false
	}
	return text, true
}

func displayName(name string) string {
	if len(name) > 64 {
		return name[:64] + "…"
	}
	return name
}

// FormatMilli renders milli-CPUs the way docker run --cpus expects them.
func FormatMilli(milli int) string {
	return strconv.FormatFloat(float64(milli)/1000, 'f', -1, 64)
}
