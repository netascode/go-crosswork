package crosswork

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// YangPatchEdit is one edit of a YANG-PATCH (RFC 8072) request. The names match go-restconf
// so code written for it keeps compiling.
type YangPatchEdit struct {
	// Operation is one of create, delete, insert, merge, move, replace or remove.
	Operation string
	// Target is the path of the data node, relative to the patch path, e.g. /tailf-ncs:devices/authgroups/group=g1.
	Target string
	// Value is the JSON data of the edit; leave it empty for remove and delete.
	Value Body
}

// NewYangPatchEdit creates a YangPatchEdit for YangPatch.
func NewYangPatchEdit(operation, target string, value Body) YangPatchEdit {
	return YangPatchEdit{Operation: operation, Target: target, Value: value}
}

type yangPatchRoot struct {
	YangPatch yangPatch `json:"ietf-yang-patch:yang-patch"`
}

type yangPatch struct {
	PatchID string          `json:"patch-id"`
	Comment string          `json:"comment,omitempty"`
	Edit    []yangPatchEdit `json:"edit"`
}

type yangPatchEdit struct {
	EditID    string          `json:"edit-id"`
	Operation string          `json:"operation"`
	Target    string          `json:"target"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// YangPatch makes a YANG-PATCH (RFC 8072) request and returns a gjson result. The edits are
// applied atomically: if one fails, none is applied. It is the way to batch many changes into
// one call.
//
//	client.YangPatch("/crosswork/proxy/nso/restconf/data", "patch-1", "",
//	    []YangPatchEdit{NewYangPatchEdit("merge", "/tailf-ncs:devices/authgroups/group=g1", Body{}.SetRaw(...))})
//
// The request is sent as application/yang-patch+json and answered as application/yang-data+json.
// PATCH is not retried after an ambiguous failure unless RetryNonIdempotent is passed.
func (client *Client) YangPatch(path, patchID, comment string, edits []YangPatchEdit, mods ...func(*Req)) (Res, error) {
	root := yangPatchRoot{YangPatch: yangPatch{PatchID: patchID, Comment: comment}}
	for i, e := range edits {
		pe := yangPatchEdit{EditID: strconv.Itoa(i), Operation: e.Operation, Target: e.Target}
		if strings.TrimSpace(e.Value.Str) != "" {
			pe.Value = json.RawMessage(e.Value.Str)
		}
		root.YangPatch.Edit = append(root.YangPatch.Edit, pe)
	}
	data, err := json.Marshal(root)
	if err != nil {
		return Res{}, err
	}
	return client.Patch(path, string(data), append([]func(*Req){ContentType(patchType)}, mods...)...)
}

// NSORetry enables the NSO busy policy for write calls under /crosswork/proxy/nso/.
// It is off by default.
//
// When NSO rejects a write because another transaction holds the datastore lock, nothing was
// applied, so the write is safe to retry whatever its method. The policy recognises that
// from the structured RESTCONF error-tag, `lock-denied` or `in-use`, never from message text
// (go-restconf also matches IOS-XE and "sync in progress" messages; those are not ported).
// It then waits, polling once a second, until the running datastore has no lock or
// timeoutSeconds have passed (go-restconf's default is 120), and retries with the normal
// backoff, up to MaxRetries times.
func NSORetry(timeoutSeconds int) func(*Client) {
	return func(client *Client) {
		client.NSOLockTimeout = timeoutSeconds
	}
}

const nsoProxyPrefix = "/crosswork/proxy/nso/restconf"

// nsoBusy reports whether a failed response is NSO saying it is busy: a write under the NSO
// proxy that fails with the RESTCONF error-tag lock-denied or in-use.
func (client *Client) nsoBusy(req Req, status int, raw []byte) bool {
	if client.NSOLockTimeout <= 0 || req.noNSORetry || req.HttpReq.Method == http.MethodGet || status < 400 {
		return false
	}
	if !strings.HasPrefix(matchPath(req.HttpReq.URL), nsoProxyPrefix) {
		return false
	}
	_, _, rc := parseError(string(raw))
	for _, e := range rc {
		if e.Tag == "lock-denied" || e.Tag == "in-use" {
			return true
		}
	}
	return false
}

// waitForNSOLock polls the NSO datastore state until the running datastore is not locked, the
// timeout passes, the lock state cannot be read, or ctx is cancelled.
func (client *Client) waitForNSOLock(ctx context.Context, req Req) {
	u := *req.HttpReq.URL
	i := strings.Index(u.Path, "/restconf")
	if i < 0 {
		return
	}
	u.Path = u.Path[:i] + "/restconf/data/ietf-netconf-monitoring:netconf-state/datastores/datastore"
	u.RawQuery = ""
	deadline := time.Now().Add(time.Duration(client.NSOLockTimeout) * time.Second)
	for {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return
		}
		res, err := client.Do(Req{HttpReq: r, LogPayload: true, noNSORetry: true, noResultCheck: true})
		if err != nil || !nsoLocked(res) {
			return // free, or the state cannot be read: do not hang, just retry
		}
		if time.Now().After(deadline) {
			log.Printf("[WARNING] NSO datastore is still locked after %d seconds, retrying anyway", client.NSOLockTimeout)
			return
		}
		log.Printf("[DEBUG] NSO datastore is locked, waiting")
		if !sleep(ctx, time.Second) {
			return
		}
	}
}

// nsoLocked reports whether the `running` datastore of an ietf-netconf-monitoring datastore
// list has a partial or global lock.
func nsoLocked(res Res) bool {
	locked := false
	res.Get(`ietf-netconf-monitoring:datastore`).ForEach(func(_, ds gjson.Result) bool {
		if ds.Get("name").String() != "running" {
			return true
		}
		if pl := ds.Get("locks.partial-lock"); pl.IsArray() && len(pl.Array()) > 0 || ds.Get("locks.global-lock").Exists() {
			locked = true
		}
		return false
	})
	return locked
}
