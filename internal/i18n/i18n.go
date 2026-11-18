package i18n

import (
	"os"
	"strings"
)

// Lang represents a supported UI language code.
type Lang string

const (
	LangCA Lang = "ca"
	LangES Lang = "es"
	LangEN Lang = "en"
)

// Normalize coerces a language or locale string (e.g. "ca_ES.UTF-8", "es-ES", "en")
// into a supported Lang, returning fallback if unrecognized.
func Normalize(raw string, fallback Lang) Lang {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return fallback
	}
	if idx := strings.IndexAny(s, "_-."); idx >= 0 {
		s = s[:idx]
	}
	switch s {
	case "ca":
		return LangCA
	case "es":
		return LangES
	case "en":
		return LangEN
	default:
		return fallback
	}
}

// Detect inspects GECKO_LANG, LC_ALL, LC_MESSAGES, and LANG environment variables
// and falls back to defaultLang if none match a supported language.
func Detect(defaultLang Lang) Lang {
	for _, k := range []string{"GECKO_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			if l := Normalize(v, ""); l != "" {
				return l
			}
		}
	}
	return defaultLang
}

// ClientStrings holds localized text for the portable Client UI across Windows, Linux, and macOS.
type ClientStrings struct {
	WindowTitle          string
	Connecting           string
	Reconnecting         string
	Online               string
	ActiveSession        string
	PromptMessage        string
	Allow                string
	Deny                 string
	Disconnect           string
	NoConfig             string
	InvalidCert          string
	AccessRejected       string
	BusyReason           string
	TimeoutReason        string
	DeniedByUserReason   string
	DisconnectedByUser   string
	ClientClosedReason   string
	WaylandNoX11Warning  string
	WaylandXWaylandWarn  string
	NoX11DisplayWarning  string
	ErrorFallback        string
}

// Client returns the localized ClientStrings for lang.
func Client(lang Lang) ClientStrings {
	switch Normalize(string(lang), LangCA) {
	case LangES:
		return ClientStrings{
			WindowTitle:         "Cliente Remoto",
			Connecting:          "Conectando...",
			Reconnecting:        "Reconectando...",
			Online:              "En línea",
			ActiveSession:       "Sesión activa",
			PromptMessage:       "El administrador solicita controlar este equipo.",
			Allow:               "Permitir",
			Deny:                "Denegar",
			Disconnect:          "Desconectar",
			NoConfig:            "Sin configuración",
			InvalidCert:         "Certificado no válido",
			AccessRejected:      "Acceso rechazado",
			BusyReason:          "El equipo ya está ocupado",
			TimeoutReason:       "Tiempo de espera agotado",
			DeniedByUserReason:  "Denegado por el usuario",
			DisconnectedByUser:  "Desconectado por el usuario",
			ClientClosedReason:  "Cliente cerrado",
			WaylandNoX11Warning: "Sesión Wayland: requiere grim/ydotool o X11",
			WaylandXWaylandWarn: "Wayland detectado: usando modo compatible",
			NoX11DisplayWarning: "No se detectó entorno gráfico X11/Wayland",
			ErrorFallback:       "Error",
		}
	case LangEN:
		return ClientStrings{
			WindowTitle:         "Remote Client",
			Connecting:          "Connecting...",
			Reconnecting:        "Reconnecting...",
			Online:              "Online",
			ActiveSession:       "Active session",
			PromptMessage:       "An administrator is requesting to control this computer.",
			Allow:               "Allow",
			Deny:                "Deny",
			Disconnect:          "Disconnect",
			NoConfig:            "Missing configuration",
			InvalidCert:         "Invalid certificate",
			AccessRejected:      "Access rejected",
			BusyReason:          "This computer is already busy",
			TimeoutReason:       "Request timed out",
			DeniedByUserReason:  "Denied by user",
			DisconnectedByUser:  "Disconnected by user",
			ClientClosedReason:  "Client closed",
			WaylandNoX11Warning: "Wayland session: needs grim/ydotool or X11",
			WaylandXWaylandWarn: "Wayland detected: using compatibility mode",
			NoX11DisplayWarning: "No X11/Wayland graphical session found",
			ErrorFallback:       "Error",
		}
	default: // LangCA
		return ClientStrings{
			WindowTitle:         "Client Remot",
			Connecting:          "Connectant...",
			Reconnecting:        "Reconnectant...",
			Online:              "En línia",
			ActiveSession:       "Sessió activa",
			PromptMessage:       "L'administrador sol·licita controlar aquest equip.",
			Allow:               "Permetre",
			Deny:                "Denegar",
			Disconnect:          "Desconnectar",
			NoConfig:            "Sense configuració",
			InvalidCert:         "Certificat no vàlid",
			AccessRejected:      "Accés rebutjat",
			BusyReason:          "L'equip ja està ocupat",
			TimeoutReason:       "Temps d'espera esgotat",
			DeniedByUserReason:  "Denegat per l'usuari",
			DisconnectedByUser:  "Desconnectat per l'usuari",
			ClientClosedReason:  "Client tancat",
			WaylandNoX11Warning: "Sessió Wayland: cal grim/ydotool o X11",
			WaylandXWaylandWarn: "Wayland detectat: mode compatible actiu",
			NoX11DisplayWarning: "No s'ha trobat cap sessió gràfica X11/Wayland",
			ErrorFallback:       "Error",
		}
	}
}
