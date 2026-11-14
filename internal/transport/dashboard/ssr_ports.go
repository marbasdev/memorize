package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

func buildPortsModalData(r *http.Request, d Deps, lang string) ssrPortsModalData {
	dbPath := d.Memory.DBPath()
	portCfg := config.LoadPortConfig(dbPath)
	data := ssrPortsModalData{
		Lang:             lang,
		ConfiguredDaemon: portCfg.DaemonPort,
		ActiveDaemon:     portCfg.DaemonPort,
		ConfiguredGGUF:   portCfg.GGUFPort,
		ActiveGGUF:       portCfg.GGUFPort,
	}

	if d.Hooks.DaemonStatusFn != nil {
		ds := d.Hooks.DaemonStatusFn()
		if ds.ConfiguredPort > 0 {
			data.ConfiguredDaemon = ds.ConfiguredPort
		}
		if ds.ActivePort > 0 {
			data.ActiveDaemon = ds.ActivePort
		} else if _, p, ok := domain.ParseHostPort(ds.ListenAddr); ok && p > 0 {
			data.ActiveDaemon = p
		}
		data.ListenAddr = ds.ListenAddr
		data.DaemonPID = ds.PID
		data.DaemonRunning = ds.PID > 0 || ds.ListenAddr != ""
		data.DaemonFallback = ds.Fallback
	} else if st, ok := config.ReadDaemonState(dbPath); ok && st.ListenAddr != "" {
		data.ListenAddr = st.ListenAddr
		data.DaemonPID = st.PID
		data.DaemonRunning = true
		if _, p, okp := domain.ParseHostPort(st.ListenAddr); okp && p > 0 {
			data.ActiveDaemon = p
			data.DaemonFallback = p != data.ConfiguredDaemon
		}
	} else if r != nil && r.Host != "" {
		data.ListenAddr = r.Host
		if _, p, okp := domain.ParseHostPort(r.Host); okp && p > 0 {
			data.ActiveDaemon = p
			data.DaemonFallback = p != data.ConfiguredDaemon
		}
	}

	if d.LocalModel != nil {
		lm := d.LocalModel.Status(portCfg.GGUFPort)
		data.GGUFSupported = lm.Supported
		data.GGUFRunning = lm.ServerRunning
		data.GGUFEndpoint = lm.Endpoint
		if lm.Port > 0 {
			data.ActiveGGUF = lm.Port
		}
		data.GGUFFallback = lm.PortFallback
	}
	return data
}

func setHXPortTriggers(w http.ResponseWriter, toast, redirectURL string, closeModal bool) {
	payload := map[string]any{
		"memorizeToast": toast,
	}
	if closeModal {
		payload["memorizeCloseModal"] = true
	}
	if strings.TrimSpace(redirectURL) != "" {
		payload["memorizePortRedirect"] = map[string]string{
			"url": strings.TrimSpace(redirectURL),
		}
	}
	if b, err := json.Marshal(payload); err == nil {
		w.Header().Set("HX-Trigger", escapeJSONASCII(b))
	}
}

func registerSSRPortRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/modal/ports", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		data := buildPortsModalData(r, d, lang)
		writeHTMLTemplate(w, "modal_ports", data)
	}))

	mux.HandleFunc("/ui/ports/save", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		var dp, gp int
		if r.FormValue("reset") == "1" {
			dp = domain.DefaultDaemonPort
			gp = domain.DefaultGGUFPort
		} else {
			var err1, err2 error
			dp, err1 = strconv.Atoi(strings.TrimSpace(r.FormValue("daemon_port")))
			gp, err2 = strconv.Atoi(strings.TrimSpace(r.FormValue("gguf_port")))
			if err1 != nil || err2 != nil {
				http.Error(w, "ports must be valid integers between 1 and 65535", http.StatusBadRequest)
				return
			}
		}
		if err := domain.ValidatePortConfig(dp, gp); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := config.SavePortConfig(d.Memory.DBPath(), domain.PortConfig{DaemonPort: dp, GGUFPort: gp}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// ApplyDaemonPortFn handles coordinated GGUF restart/sync + daemon port rebind when inside daemon.
		if d.Hooks.ApplyDaemonPortFn != nil {
			res, err := d.Hooks.ApplyDaemonPortFn(r.Context(), dp, gp)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			activeGGUF := res.ActiveGGUFPort
			if activeGGUF == 0 {
				activeGGUF = gp
			}
			toast := fmt.Sprintf("Ports saved (Daemon :%d · GGUF :%d)", dp, activeGGUF)
			if lang == "ko" {
				toast = fmt.Sprintf("포트 설정 저장됨 (데몬 :%d · GGUF :%d)", dp, activeGGUF)
			}
			if res.Restarting && res.NewURL != "" {
				if lang == "ko" {
					toast = fmt.Sprintf("데몬 포트를 %s로 변경하여 재기동합니다...", res.NewURL)
				} else {
					toast = fmt.Sprintf("Restarting daemon on %s...", res.NewURL)
				}
				setHXPortTriggers(w, toast, res.NewURL, true)
				renderEnginesSection(w, r, d, lang, url.Values{"view": {"gguf"}}, "", "")
				return
			}
			setHXPortTriggers(w, toast, "", true)
			renderEnginesSection(w, r, d, lang, url.Values{"view": {"gguf"}}, "", "")
			return
		}

		// Standalone handler mode (e.g., tests or -dashboard without daemon wrapper).
		effGGUFPort := gp
		if d.LocalModel != nil {
			lm := d.LocalModel.Status(gp)
			if lm.ServerRunning && lm.Supported && lm.ActiveModel != "" && lm.Port != gp {
				if ep, err := d.LocalModel.StartServer(context.Background(), lm.ActiveModel, gp); err == nil && ep != "" {
					if _, p, ok := domain.ParseHostPort(ep); ok && p > 0 {
						effGGUFPort = p
					}
				}
			}
		}
		_ = config.SyncManagedGGUFEndpoint(r.Context(), d.Memory.DB(), d.Memory.DBPath(), fmt.Sprintf("http://127.0.0.1:%d", effGGUFPort))
		toast := fmt.Sprintf("Ports saved (Daemon :%d · GGUF :%d)", dp, effGGUFPort)
		if lang == "ko" {
			toast = fmt.Sprintf("포트 설정 저장됨 (데몬 :%d · GGUF :%d)", dp, effGGUFPort)
		}
		setHXPortTriggers(w, toast, "", true)
		renderEnginesSection(w, r, d, lang, url.Values{"view": {"gguf"}}, "", "")
	}))
}
