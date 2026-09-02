package mcp

import (
	"encoding/json"
	"testing"
)

// mustJSON decodes a literal object the way the dynamic client hands one over.
func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return m
}

func get(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, p)
		}
		cur = mm[p]
	}
	return cur
}

func TestRedactsInlineEnvValue(t *testing.T) {
	obj := mustJSON(t, `{"spec":{"containers":[
		{"name":"app","env":[
			{"name":"DB_PASSWORD","value":"hunter2"},
			{"name":"API_TOKEN","value":"abc123"},
			{"name":"LOG_LEVEL","value":"debug"}
		]}
	]}}`)

	if n := redactObject(obj); n != 2 {
		t.Fatalf("redacted %d values, want 2", n)
	}
	env := get(t, obj, "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	if v := env[0].(map[string]any); v["value"] != redacted {
		t.Errorf("DB_PASSWORD not redacted: %v", v)
	} else if v["name"] != "DB_PASSWORD" {
		t.Errorf("variable name lost: %v", v)
	}
	if v := env[2].(map[string]any); v["value"] != "debug" {
		t.Errorf("LOG_LEVEL was redacted: %v", v)
	}
}

// A secretKeyRef is a reference, not a credential. Blanking it would cost the
// answer to "which secret does this come from".
func TestKeepsSecretReferences(t *testing.T) {
	obj := mustJSON(t, `{"spec":{
		"tls":[{"secretName":"web-tls","hosts":["a.example.com"]}],
		"containers":[{"env":[{"name":"PASSWORD","valueFrom":{"secretKeyRef":{"name":"db-creds","key":"password"}}}]}],
		"remoteWrite":[{"url":"https://vm.internal/api/v1/write","basicAuth":{"password":{"name":"vm-creds","key":"pass"}}}]
	}}`)

	redactObject(obj)

	tls := get(t, obj, "spec").(map[string]any)["tls"].([]any)[0].(map[string]any)
	if tls["secretName"] != "web-tls" {
		t.Errorf("secretName redacted: %v", tls)
	}
	ref := get(t, obj, "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)[0].(map[string]any)["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
	if ref["name"] != "db-creds" || ref["key"] != "password" {
		t.Errorf("secretKeyRef mangled: %v", ref)
	}
	rw := get(t, obj, "spec").(map[string]any)["remoteWrite"].([]any)[0].(map[string]any)
	if rw["url"] != "https://vm.internal/api/v1/write" {
		t.Errorf("credential-free URL altered: %v", rw["url"])
	}
	auth := rw["basicAuth"].(map[string]any)["password"].(map[string]any)
	if auth["name"] != "vm-creds" || auth["key"] != "pass" {
		t.Errorf("basicAuth secret reference mangled: %v", auth)
	}
}

func TestRedactsURLUserinfoKeepingEndpoint(t *testing.T) {
	obj := mustJSON(t, `{"spec":{"remoteWrite":[{"url":"https://writer:s3cr3t@vm.internal:8428/api/v1/write"}]}}`)

	if n := redactObject(obj); n != 1 {
		t.Fatalf("redacted %d, want 1", n)
	}
	got := get(t, obj, "spec").(map[string]any)["remoteWrite"].([]any)[0].(map[string]any)["url"]
	want := "https://writer:" + redacted + "@vm.internal:8428/api/v1/write"
	if got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestRedactsSensitiveFlagValues(t *testing.T) {
	obj := mustJSON(t, `{"spec":{
		"extraArgs":{"remoteWrite.basicAuth.password":"hunter2","memory.allowedPercent":"60"},
		"args":["-remoteWrite.basicAuth.password=hunter2","-promscrape.config=/cfg/scrape.yml"]
	}}`)

	redactObject(obj)

	extra := get(t, obj, "spec").(map[string]any)["extraArgs"].(map[string]any)
	if extra["remoteWrite.basicAuth.password"] != redacted {
		t.Errorf("extraArgs password not redacted: %v", extra)
	}
	if extra["memory.allowedPercent"] != "60" {
		t.Errorf("harmless extraArg redacted: %v", extra)
	}
	args := get(t, obj, "spec").(map[string]any)["args"].([]any)
	if args[0] != "-remoteWrite.basicAuth.password="+redacted {
		t.Errorf("flag not redacted: %v", args[0])
	}
	if args[1] != "-promscrape.config=/cfg/scrape.yml" {
		t.Errorf("harmless flag altered: %v", args[1])
	}
}

// A Route can carry an inline private key under the very common field name
// "key", recognised only by its TLS siblings. The certificate is public and
// must survive: expiry and subject are what users ask about.
func TestRedactsInlineRouteTLSKeyKeepingCertificate(t *testing.T) {
	obj := mustJSON(t, `{"spec":{"host":"app.example.com","tls":{
		"termination":"edge",
		"certificate":"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		"key":"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----"
	}}}`)

	if n := redactObject(obj); n != 1 {
		t.Fatalf("redacted %d, want 1", n)
	}
	tls := get(t, obj, "spec").(map[string]any)["tls"].(map[string]any)
	if tls["key"] != redacted {
		t.Errorf("private key not redacted: %v", tls["key"])
	}
	if tls["certificate"] == redacted {
		t.Error("public certificate was redacted")
	}
	if tls["termination"] != "edge" {
		t.Errorf("termination altered: %v", tls["termination"])
	}
}

// The diagnostic content of an object must survive untouched — this is the
// guard against redaction quietly degrading answers.
func TestLeavesDiagnosticFieldsAlone(t *testing.T) {
	obj := mustJSON(t, `{
		"kind":"Deployment",
		"metadata":{"name":"web","namespace":"team-a","resourceVersion":"88213",
			"uid":"3f2b1a90-1c7d-4f0e-9a2b-0c1d2e3f4a5b",
			"labels":{"app":"web","app.kubernetes.io/instance":"web-prod"}},
		"spec":{"replicas":3,"template":{"spec":{"containers":[{
			"name":"web",
			"image":"registry.example.com/web@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"ports":[{"containerPort":8080}],
			"resources":{"limits":{"memory":"512Mi"}}
		}]}}},
		"status":{"readyReplicas":2,"conditions":[{"type":"Available","status":"False","reason":"MinimumReplicasUnavailable"}]}
	}`)
	before, _ := json.Marshal(obj)

	if n := redactObject(obj); n != 0 {
		t.Fatalf("redacted %d values in a credential-free object", n)
	}
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Errorf("object mutated:\nbefore %s\nafter  %s", before, after)
	}
}

func TestSensitiveKeyNaming(t *testing.T) {
	sensitive := []string{"password", "DB_PASSWORD", "db-password", "clientSecret",
		"bearerToken", "apiKey", "aws_access_key_id", "privateKey", "authorization"}
	for _, k := range sensitive {
		if !sensitiveKey(k) {
			t.Errorf("sensitiveKey(%q) = false, want true", k)
		}
	}
	// Names that look sensitive but identify a resource or a key inside one.
	benign := []string{"secretName", "secretKeyRef", "existingSecret", "configSecret",
		"imagePullSecrets", "name", "key", "serviceAccountName", "tokenSecret"}
	for _, k := range benign {
		if sensitiveKey(k) {
			t.Errorf("sensitiveKey(%q) = true, want false", k)
		}
	}
}
