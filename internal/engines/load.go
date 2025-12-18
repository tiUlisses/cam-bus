package engines

import (
    "log"
    "os"
    "strconv"
    "strings"
    "time"
)

// LoadFromEnv carrega as engines habilitadas.
//
// Preferencial: ENGINES="findface,plater" (comma-separated)
// Compatibilidade: se ENGINES não vier, usa FACE_ENGINE (quando for "findface").
func LoadFromEnv() *Manager {
    names := parseCSV(os.Getenv("ENGINES"))
    if len(names) == 0 {
        // compat: comportamento antigo
        fe := strings.ToLower(strings.TrimSpace(os.Getenv("FACE_ENGINE")))
        if fe != "" && fe != "none" {
            names = []string{fe}
        }
    }

    timeout := envDurationSeconds("ENGINE_TIMEOUT_SECONDS", 10*time.Second)

    var list []Engine
    for _, n := range names {
        switch strings.ToLower(n) {
        case "findface":
            if e := NewFindFaceFromEnv(); e != nil && e.Enabled() {
                list = append(list, e)
            }
        case "plater", "plate", "lpr":
            // Placeholder: mantém a arquitetura modular pronta.
            // Implementaremos de verdade quando definirmos o provider (ex.: Plate Recognizer / OpenALPR / engine nativa).
            list = append(list, NewPlateStub())
        default:
            log.Printf("[engines] engine %q desconhecida (ignorando)", n)
        }
    }

    m := NewManager(list, timeout)
    if m.Enabled() {
        log.Printf("[engines] habilitadas: %s", strings.Join(m.Names(), ","))
    } else {
        log.Printf("[engines] nenhuma engine habilitada")
    }
    return m
}

func parseCSV(v string) []string {
    v = strings.TrimSpace(v)
    if v == "" {
        return nil
    }
    parts := strings.Split(v, ",")
    out := make([]string, 0, len(parts))
    for _, p := range parts {
        s := strings.TrimSpace(p)
        if s == "" {
            continue
        }
        out = append(out, s)
    }
    return out
}

func envDurationSeconds(key string, def time.Duration) time.Duration {
    v := strings.TrimSpace(os.Getenv(key))
    if v == "" {
        return def
    }
    sec, err := strconv.Atoi(v)
    if err != nil || sec <= 0 {
        return def
    }
    return time.Duration(sec) * time.Second
}
