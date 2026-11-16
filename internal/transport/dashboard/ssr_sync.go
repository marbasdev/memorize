package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"memorize/internal/domain"
)

type ssrSyncModalData struct {
	Lang            string
	SelfDeviceID    string
	SelfDeviceLabel string
	Peers           []domain.SyncPeer
	ActivePeer      domain.SyncPeer
	ScopeChecked    map[string]bool
	KnownScopes     []string
	Conflicts       []domain.SyncConflict
	Probe           *domain.SyncPeerProbe
	Report          *domain.SyncRunReport
	ErrorMsg        string
}

func buildSyncModalData(r *http.Request, d Deps, lang, selectedPeer string) ssrSyncModalData {
	ctx := r.Context()
	selfDev, selfLbl := d.Memory.SelfDeviceID(ctx)
	peers, _ := d.Memory.ListSyncPeers(ctx)
	knownScopes := d.Memory.ListKnownSyncScopes(ctx)
	conflicts, _ := d.Memory.ListSyncConflicts(ctx)

	active := domain.SyncPeer{
		ScopeMode: domain.SyncScopeModeAll,
		Scopes:    []string{"global"},
	}
	sel := strings.TrimSpace(selectedPeer)
	if sel != "__new__" {
		for _, p := range peers {
			if sel == "" || p.PeerID == sel || p.SSHTarget == sel {
				active = p
				break
			}
		}
	}
	return finalizeSyncModalData(ssrSyncModalData{
		Lang:            lang,
		SelfDeviceID:    selfDev,
		SelfDeviceLabel: selfLbl,
		Peers:           peers,
		ActivePeer:      active,
		KnownScopes:     knownScopes,
		Conflicts:       conflicts,
	})
}

func finalizeSyncModalData(data ssrSyncModalData) ssrSyncModalData {
	mode, scopes := domain.NormalizeSyncScopes(data.ActivePeer.ScopeMode, data.ActivePeer.Scopes)
	data.ActivePeer.ScopeMode = mode
	data.ActivePeer.Scopes = scopes

	checked := make(map[string]bool, len(scopes))
	for _, sc := range scopes {
		checked[sc] = true
	}
	data.ScopeChecked = checked

	knownSet := make(map[string]bool, len(data.KnownScopes))
	for _, sc := range data.KnownScopes {
		knownSet[sc] = true
	}
	for _, sc := range scopes {
		if !knownSet[sc] {
			data.KnownScopes = append(data.KnownScopes, sc)
			knownSet[sc] = true
		}
	}
	return data
}

func parseSyncPeerForm(r *http.Request) domain.SyncPeer {
	_ = r.ParseForm()
	mode, scopes := domain.NormalizeSyncScopes(r.FormValue("scope_mode"), r.Form["scopes"])
	return domain.SyncPeer{
		PeerID:      strings.TrimSpace(r.FormValue("peer_id")),
		SSHTarget:   strings.TrimSpace(r.FormValue("ssh_target")),
		RemoteBin:   strings.TrimSpace(r.FormValue("remote_bin")),
		DeviceLabel: strings.TrimSpace(r.FormValue("device_label")),
		ScopeMode:   mode,
		Scopes:      scopes,
	}
}

func requireSyncEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !domain.IsMemorySyncEnabled() {
			http.Error(w, domain.ErrMemorySyncDisabled.Error(), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func registerSSRSyncRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/modal/sync", requireSyncEnabled(getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		data := buildSyncModalData(r, d, lang, r.URL.Query().Get("peer"))
		if r.URL.Query().Get("body") == "1" {
			writeHTMLTemplate(w, "modal_sync_body", data)
			return
		}
		writeHTMLTemplate(w, "modal_sync", data)
	})))

	mux.HandleFunc("/ui/sync/peer/save", requireSyncEnabled(postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		formPeer := parseSyncPeerForm(r)
		saved, err := d.Memory.UpsertSyncPeer(r.Context(), formPeer)
		if err != nil {
			data := buildSyncModalData(r, d, lang, formPeer.PeerID)
			data.ActivePeer = formPeer
			data.ErrorMsg = err.Error()
			writeHTMLTemplate(w, "modal_sync_body", finalizeSyncModalData(data))
			return
		}
		setHXTriggers(w, fmt.Sprintf("Saved peer %s", saved.PeerID), false, false)
		writeHTMLTemplate(w, "modal_sync_body", buildSyncModalData(r, d, lang, saved.PeerID))
	})))

	mux.HandleFunc("/ui/sync/peer/delete", requireSyncEnabled(postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		peerID := strings.TrimSpace(r.FormValue("peer_id"))
		if err := d.Memory.DeleteSyncPeer(r.Context(), peerID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		setHXTriggers(w, fmt.Sprintf("Removed peer %s", peerID), false, false)
		writeHTMLTemplate(w, "modal_sync_body", buildSyncModalData(r, d, lang, ""))
	})))

	mux.HandleFunc("/ui/sync/test", requireSyncEnabled(postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		formPeer := parseSyncPeerForm(r)
		probe := d.Memory.TestSyncTarget(r.Context(), formPeer.SSHTarget, formPeer.RemoteBin)
		data := buildSyncModalData(r, d, lang, formPeer.PeerID)
		data.ActivePeer = formPeer
		data.Probe = &probe
		if probe.OK {
			setHXTriggers(w, fmt.Sprintf("SSH connected to %s (%s)", probe.RemoteLabel, probe.RemoteDeviceID), false, false)
		} else {
			setHXTriggers(w, "SSH probe failed", false, false)
		}
		writeHTMLTemplate(w, "modal_sync_body", finalizeSyncModalData(data))
	})))

	mux.HandleFunc("/ui/sync/run", requireSyncEnabled(postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		formPeer := parseSyncPeerForm(r)
		saved, err := d.Memory.UpsertSyncPeer(r.Context(), formPeer)
		if err != nil {
			data := buildSyncModalData(r, d, lang, formPeer.PeerID)
			data.ActivePeer = formPeer
			data.ErrorMsg = err.Error()
			writeHTMLTemplate(w, "modal_sync_body", finalizeSyncModalData(data))
			return
		}
		rep, syncErr := d.Memory.SyncWithPeerSpec(r.Context(), saved)
		data := buildSyncModalData(r, d, lang, saved.PeerID)
		if syncErr != nil {
			data.ErrorMsg = syncErr.Error()
			setHXTriggers(w, "Sync failed", false, false)
		} else {
			data.Report = &rep
			setHXTriggers(w, fmt.Sprintf("Synced: pulled %d, pushed %d, conflicts %d",
				rep.PulledApplied, rep.PushedApplied, rep.PullConflicts+rep.PushedConflicts), false, false)
		}
		writeHTMLTemplate(w, "modal_sync_body", data)
	})))

	mux.HandleFunc("/ui/sync/conflict/resolve", requireSyncEnabled(postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		syncID := strings.TrimSpace(r.FormValue("sync_id"))
		decision := strings.TrimSpace(r.FormValue("decision"))
		msg, err := d.Memory.ResolveSyncConflict(r.Context(), syncID, decision)
		if err != nil {
			if strings.TrimSpace(r.FormValue("from")) == "modal" {
				data := buildSyncModalData(r, d, lang, r.FormValue("peer_id"))
				data.ErrorMsg = err.Error()
				writeHTMLTemplate(w, "modal_sync_body", data)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(r.FormValue("from")) == "modal" {
			setHXTriggers(w, msg, false, false)
			writeHTMLTemplate(w, "modal_sync_body", buildSyncModalData(r, d, lang, r.FormValue("peer_id")))
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, msg, false, false)
	})))
}
