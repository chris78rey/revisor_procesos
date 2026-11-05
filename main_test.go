package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCrossOriginWrite(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{"GET cruzado se permite", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"POST mismo origen", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"POST escrito en la barra", "POST", map[string]string{"Sec-Fetch-Site": "none"}, false},
		{"POST de otro sitio", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"POST de otro subdominio", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"POST sin cabeceras (curl)", "POST", nil, false},
		{"Origin coincide con Host", "POST", map[string]string{"Origin": "http://127.0.0.1:8080"}, false},
		{"Origin distinto", "POST", map[string]string{"Origin": "https://evil.example"}, true},
		{"Origin nulo", "POST", map[string]string{"Origin": "null"}, true},
		{"Origin tras proxy", "POST", map[string]string{"Origin": "https://revisor.example", "X-Forwarded-Host": "revisor.example"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "http://127.0.0.1:8080/processes/delete", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := crossOriginWrite(r); got != c.want {
				t.Fatalf("crossOriginWrite = %v, esperado %v", got, c.want)
			}
		})
	}
}

func TestSecurityHeadersBlocksCrossSitePost(t *testing.T) {
	called := false
	h := securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest("POST", "/settings", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("código %d, handler llamado=%v; se esperaba 403 sin llamar al handler", w.Code, called)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Fatal("la CSP debe permitir enmarcar solo desde el mismo origen (visor de PDF)")
	}
}

func TestSelectProcessCode(t *testing.T) {
	candidates := []processCandidate{
		{code: "", object: "Compra de laptops para oficina", supplier: "Tecno SA"},
		{code: "RE-001-2026", object: "Mantenimiento de ascensores", supplier: "Elevadores del Sur"},
		{code: "RE-001", object: "Servicio de limpieza", supplier: "Limpio Cia"},
		{code: "1", object: "Papelería", supplier: "Papelera Andina"},
		{code: "SIE-7", object: "Compra de laptops para oficina", supplier: "Tecno SA"},
	}
	cases := []struct{ q, want string }{
		{"¿En qué etapa está RE-001-2026?", "RE-001-2026"},
		{"como vamos con re-001", "RE-001"},
		{"cuántos días tiene el proceso 15", ""}, // "1" no debe coincidir dentro de "15"
		{"estado del proceso 1", "1"},
		{"ascensores elevadores pendientes", "RE-001-2026"}, // por objeto y proveedor
		{"laptops oficina tecno", "SIE-7"},                  // el código vacío no corta la búsqueda
		{"qué pasa con la limpieza", ""},                    // una sola palabra no basta
		{"", ""},
	}
	for _, c := range cases {
		if got := selectProcessCode(c.q, candidates); got != c.want {
			t.Errorf("selectProcessCode(%q) = %q, esperado %q", c.q, got, c.want)
		}
	}
}

func TestSelectProcessCodeAmbiguousKeywords(t *testing.T) {
	candidates := []processCandidate{
		{code: "A-1", object: "Compra de laptops para oficina"},
		{code: "B-2", object: "Alquiler de laptops para oficina"},
	}
	if got := selectProcessCode("laptops oficina", candidates); got != "" {
		t.Fatalf("con dos procesos igual de probables no se debe elegir ninguno, se obtuvo %q", got)
	}
}

func TestQueryAsksProcessStage(t *testing.T) {
	for q, want := range map[string]bool{
		"¿En qué etapa está el proceso?": true,
		"Estado de RE-001":               true,
		"cómo vamos con el contrato":     true,
		"lista de proveedores":           false,
	} {
		if got := queryAsksProcessStage(q); got != want {
			t.Errorf("queryAsksProcessStage(%q) = %v", q, got)
		}
	}
}

func TestParseSelectedObservationProcesses(t *testing.T) {
	got := parseSelectedObservationProcesses([]string{"3", " 5 ", "3", "0", "-2", "x", "", "7"})
	if want := []int{3, 5, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, esperado %v", got, want)
	}
}

func TestEstimateExcelWrappedLines(t *testing.T) {
	cases := []struct {
		text  string
		width float64
		want  int
	}{
		{"", 40, 1},
		{"corto", 40, 1},
		{"uno\ndos\ntres", 40, 3},
		{strings.Repeat("palabra ", 30), 20, 15}, // 22 caracteres por línea: 2 palabras
		{strings.Repeat("x", 100), 10, 10},
	}
	for _, c := range cases {
		if got := estimateExcelWrappedLines(c.text, c.width); got != c.want {
			t.Errorf("estimateExcelWrappedLines(%.20q, %v) = %d, esperado %d", c.text, c.width, got, c.want)
		}
	}
}

func TestTelegramErrorForLogNeverLeaksToken(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	transport := &url.Error{Op: "Get", URL: "https://api.telegram.org/bot" + token + "/getMe", Err: errors.New("timeout")}
	if got := telegramErrorForLog(transport); strings.Contains(got, token) || strings.Contains(got, "api.telegram.org") {
		t.Fatalf("el log filtra el token: %q", got)
	}
	if got := telegramErrorForLog(errTelegramWebhook); got != errTelegramWebhook.Error() {
		t.Fatalf("el aviso del webhook debe verse completo, se obtuvo %q", got)
	}
	wrapped := fmt.Errorf("sesión: %w", errTelegramCursor)
	if got := telegramErrorForLog(wrapped); !strings.Contains(got, "cursor") {
		t.Fatalf("el error del cursor debe verse, se obtuvo %q", got)
	}
}

func TestGroupDocuments(t *testing.T) {
	docs := []DocumentVersion{
		{ID: 1, Category: "anexo-bbb", Label: "Pliego", Version: 1, Filename: "pliego.pdf"},
		{ID: 2, Category: "contrato", Version: 1, Filename: "c1.pdf"},
		{ID: 3, Category: "orden", Version: 2, Filename: "o2.png"},
		{ID: 4, Category: "orden", Version: 1, Filename: "o1.pdf"},
		{ID: 5, Category: "anexo-aaa", Label: "Acta", Version: 1, Filename: "acta.docx"},
		{ID: 6, Category: "orden", Version: 3, Filename: "o3.pdf"},
		{ID: 7, Category: "anexo-ccc", Label: "Factura 2", DocType: "Facturas", Version: 1, Filename: "f2.pdf"},
		{ID: 8, Category: "anexo-ddd", Label: "Factura 1", DocType: "Facturas", Version: 1, Filename: "f1.pdf"},
		{ID: 9, Category: "anexo-ddd", Label: "Factura 1", DocType: "Facturas", Version: 2, Filename: "f1b.pdf"},
	}
	groups := groupDocuments(docs)
	var titles []string
	for _, g := range groups {
		titles = append(titles, g.Title)
	}
	if want := []string{"Orden de compra", "Contrato", "Factura 1", "Factura 2", "Acta", "Pliego"}; !reflect.DeepEqual(titles, want) {
		t.Fatalf("orden de grupos %v, esperado %v", titles, want)
	}
	orden := groups[0]
	if len(orden.Versions) != 3 || orden.Versions[0].Version != 3 || orden.Versions[2].Version != 1 {
		t.Fatalf("versiones mal ordenadas: %+v", orden.Versions)
	}
	if !orden.Versions[0].Latest || orden.Versions[1].Latest || orden.Latest.ID != 6 {
		t.Fatalf("solo la versión mayor debe ser vigente: %+v", orden.Versions)
	}
	if orden.Versions[1].Kind != "image" || orden.Versions[0].Kind != "pdf" || groups[4].Versions[0].Kind != "" {
		t.Fatal("el tipo de visor no se calculó bien")
	}
	if groups[2].Type != "Facturas" || groups[2].Rank != 3 || groups[2].Key != "t:facturas|factura 1" || len(groups[2].Versions) != 2 {
		t.Fatalf("las facturas deben agruparse por documento dentro del tipo: %+v", groups[2])
	}
	if groups[4].Type != "Anexos" || groups[4].Rank != 4 || groups[4].Key != "anexo:acta" || groups[0].Type != "Orden de compra" || groups[0].Key != "orden" {
		t.Fatalf("tipos de los demás grupos: %+v %+v", groups[0], groups[4])
	}
}

func TestDocumentSpecFromForm(t *testing.T) {
	a := newTestApp(t)
	// Tipos estándar: el nombre se ignora.
	if sp, err := a.documentSpecFromForm("contrato", "ignorado", ""); err != nil || sp.Category != "contrato" || sp.Label != "" || sp.DocType != "" {
		t.Fatalf("contrato: %+v %v", sp, err)
	}
	// Un tipo predefinido o creado: tipo + nombre identifican al documento.
	f1, err := a.documentSpecFromForm("t:Facturas", " Factura 001 ", "")
	f2, _ := a.documentSpecFromForm("t:facturas", "factura 001", "")
	f3, _ := a.documentSpecFromForm("t:Facturas", "Factura 002", "")
	notif, _ := a.documentSpecFromForm("t:Notificaciones", "Factura 001", "")
	if err != nil || f1.DocType != "Facturas" || f1.Label != "Factura 001" || !strings.HasPrefix(f1.Category, "anexo-") || len(f1.Category) != 18 {
		t.Fatalf("factura: %+v %v", f1, err)
	}
	if f1.Category != f2.Category || f2.DocType != "Facturas" {
		t.Fatal("mismo tipo y nombre (sin importar mayúsculas) deben ser el mismo documento, con la escritura canónica del tipo")
	}
	if f1.Category == f3.Category || f1.Category == notif.Category {
		t.Fatal("otro nombre u otro tipo deben ser otro documento")
	}
	// Tipo nuevo: se crea y luego se reutiliza sin duplicarse por mayúsculas o tildes.
	g, err := a.documentSpecFromForm("__new", "Póliza 1", "Garantías")
	if err != nil || g.DocType != "Garantías" {
		t.Fatalf("tipo nuevo: %+v %v", g, err)
	}
	if _, err = a.db.Exec(`INSERT INTO processes(id,code,object,created_at) VALUES(1,'X','x','2026-11-01 09:00:00')`); err != nil {
		t.Fatal(err)
	}
	a.testTypedDocument(t, g, 1, "poliza.pdf")
	if again, _ := a.documentSpecFromForm("__new", "Póliza 2", "garantias"); again.DocType != "Garantías" {
		t.Fatalf("debe reutilizar «Garantías», se obtuvo %q", again.DocType)
	}
	if !reflect.DeepEqual(a.documentTypeOptions(), []string{"Facturas", "Notificaciones", "Informes", "Actas", "Garantías"}) {
		t.Fatalf("opciones de tipo: %v", a.documentTypeOptions())
	}
	// Escribir un nombre de tipo estándar no crea un tipo paralelo.
	for name, want := range map[string]string{"Contrato": "contrato", "orden de compra": "orden", "Términos de referencia": "terminos"} {
		if sp, err := a.documentSpecFromForm("__new", "", name); err != nil || sp.Category != want {
			t.Errorf("%q: %+v %v", name, sp, err)
		}
	}
	// «Anexos» es el grupo sin tipo (compatible con los anexos antiguos).
	legacy, _ := a.documentSpecFromForm("anexo", "Pliego", "")
	viaName, _ := a.documentSpecFromForm("__new", "Pliego", "Anexos")
	if legacy.DocType != "" || legacy.Category != viaName.Category {
		t.Fatalf("anexo sin tipo: %+v vs %+v", legacy, viaName)
	}
	for _, bad := range [][3]string{{"anexo", "", ""}, {"t:Facturas", "", ""}, {"t:Facturas", strings.Repeat("a", 161), ""}, {"__new", "x", ""}, {"__new", "x", strings.Repeat("t", 61)}, {"../x", "", ""}, {"anexo-abc", "", ""}, {"", "", ""}} {
		if _, err := a.documentSpecFromForm(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("se esperaba error para %q", bad)
		}
	}
}

func (a *App) testTypedDocument(t *testing.T, spec documentSpec, version int, filename string) int {
	t.Helper()
	res, err := a.db.Exec(`INSERT INTO documents(process_id,category,label,doc_type,version,filename,stored_name,created_at,signature_report) VALUES(1,?,?,?,?,?,?,'2026-11-01 10:00:00','{}')`,
		spec.Category, spec.Label, spec.DocType, version, filename, "v1_x.pdf")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func TestAdoptLegacyDocumentTypes(t *testing.T) {
	a := newTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO processes(id,code,object,created_at) VALUES(1,'X','x','2026-11-01 09:00:00')`); err != nil {
		t.Fatal(err)
	}
	fac := a.testTypedDocument(t, documentSpec{Category: "anexo-aaaaaaaaaaaa", Label: "FACTURAS"}, 1, "f.pdf")
	not := a.testTypedDocument(t, documentSpec{Category: "anexo-bbbbbbbbbbbb", Label: "Notificación"}, 1, "n.pdf")
	otro := a.testTypedDocument(t, documentSpec{Category: "anexo-cccccccccccc", Label: "2NOTIFICACIONJAIME-signed"}, 1, "o.pdf")
	std := a.testTypedDocument(t, documentSpec{Category: "contrato"}, 1, "c.pdf")
	if err := adoptLegacyDocumentTypes(a.db); err != nil {
		t.Fatal(err)
	}
	typeOf := func(id int) string {
		var d string
		a.db.QueryRow(`SELECT doc_type FROM documents WHERE id=?`, id).Scan(&d)
		return d
	}
	if typeOf(fac) != "Facturas" || typeOf(not) != "" || typeOf(otro) != "" || typeOf(std) != "" {
		t.Fatalf("migración: %q %q %q %q", typeOf(fac), typeOf(not), typeOf(otro), typeOf(std))
	}
}

func TestViewableContentType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	cases := []struct {
		name string
		head []byte
		ct   string
		ok   bool
	}{
		{"a.pdf", []byte("%PDF-1.7\n..."), "application/pdf", true},
		{"a.PDF", []byte("%PDF-1.4"), "application/pdf", true},
		{"a.pdf", []byte("<html><script>alert(1)</script>"), "", false},
		{"a.png", png, "image/png", true},
		{"a.png", []byte("<html>"), "image/png", false},
		{"a.jpg", []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), "image/jpeg", true},
		{"a.svg", []byte("<svg onload=alert(1)>"), "", false},
		{"a.html", []byte("<html>"), "", false},
		{"a.docx", []byte("PK\x03\x04"), "", false},
	}
	for _, c := range cases {
		ct, ok := viewableContentType(c.name, c.head)
		if ok != c.ok || (ok && ct != c.ct) {
			t.Errorf("%s: (%q, %v), esperado (%q, %v)", c.name, ct, ok, c.ct, c.ok)
		}
	}
}

// newTestApp crea una app con SQLite temporal y la plantilla real.
func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		t.Fatalf("la plantilla no compila: %v", err)
	}
	return &App{db: db, view: tmpl, documentDir: filepath.Join(dir, "documents")}
}

func (a *App) testDocument(t *testing.T, category, label string, version int, filename string, content []byte) int {
	t.Helper()
	stored := fmt.Sprintf("v%d_test%s", version, filepath.Ext(filename))
	dir := filepath.Join(a.documentDir, "1", category)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stored), content, 0600); err != nil {
		t.Fatal(err)
	}
	res, err := a.db.Exec(`INSERT INTO documents(process_id,category,label,version,filename,stored_name,created_at,signature_report) VALUES(1,?,?,?,?,?,?,'{}')`,
		category, label, version, filename, stored, fmt.Sprintf("2026-11-0%d 10:00:00", version))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func newTestMux(a *App) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /process", a.processDetail)
	mux.HandleFunc("GET /documents/{id}", a.downloadDocument)
	mux.HandleFunc("GET /documents/{id}/view", a.viewDocument)
	mux.HandleFunc("POST /documents/{id}/classify", a.classifyDocument)
	mux.HandleFunc("POST /documents/{id}/delete", a.deleteDocument)
	return mux
}

func TestDocumentViewerAndClassification(t *testing.T) {
	a := newTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO processes(id,code,object,created_at) VALUES(1,'RE-1','Compra','2026-11-01 09:00:00')`); err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.7\nfake")
	html := []byte("<html><script>alert(1)</script></html>")
	v1 := a.testDocument(t, "orden", "", 1, "orden-v1.pdf", pdf)
	v2 := a.testDocument(t, "orden", "", 2, "orden-v2.pdf", pdf)
	fake := a.testDocument(t, "anexo-abc", "Acta", 1, "acta.pdf", html)
	mux := newTestMux(a)
	do := func(method, target string, form url.Values) *httptest.ResponseRecorder {
		var r *http.Request
		if form != nil {
			r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, target, nil)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	// La página renderiza grupos, versión vigente y botón de visor.
	page := do("GET", "/process?id=1", nil)
	if page.Code != 200 {
		t.Fatalf("detalle: %d %s", page.Code, page.Body.String())
	}
	for _, want := range []string{`id="docGroups"`, "Vigente", fmt.Sprintf("/documents/%d/view", v2), `id="docViewer"`, "Cambiar tipo"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("la página no contiene %q", want)
		}
	}

	// El visor sirve el PDF en línea con el tipo correcto.
	view := do("GET", fmt.Sprintf("/documents/%d/view", v1), nil)
	if view.Code != 200 || view.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(view.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("visor: %d %q %q", view.Code, view.Header().Get("Content-Type"), view.Header().Get("Content-Disposition"))
	}
	// Un archivo .pdf que en realidad es HTML no se sirve en línea.
	if w := do("GET", fmt.Sprintf("/documents/%d/view", fake), nil); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("un HTML renombrado debe rechazarse, código %d", w.Code)
	}
	if w := do("GET", "/documents/999/view", nil); w.Code != 404 {
		t.Fatalf("documento inexistente: %d", w.Code)
	}

	// Reclasificar: la versión 1 de la orden pasa a ser contrato v1 y el archivo se mueve.
	w := do("POST", fmt.Sprintf("/documents/%d/classify", v1), url.Values{"category": {"contrato"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("classify: %d %s", w.Code, w.Body.String())
	}
	var category, stored string
	var version int
	if err := a.db.QueryRow(`SELECT category,version,stored_name FROM documents WHERE id=?`, v1).Scan(&category, &version, &stored); err != nil {
		t.Fatal(err)
	}
	if category != "contrato" || version != 1 {
		t.Fatalf("tras reclasificar: %s v%d", category, version)
	}
	if _, err := os.Stat(a.documentPath(1, "contrato", stored)); err != nil {
		t.Fatalf("el archivo no se movió: %v", err)
	}
	if _, err := os.Stat(a.documentPath(1, "orden", "v1_test.pdf")); !os.IsNotExist(err) {
		t.Fatal("el archivo original debería haberse movido")
	}
	// Sin nombre se usa el del archivo; con tipo creado en el momento queda en ese tipo.
	if w := do("POST", fmt.Sprintf("/documents/%d/classify", v1), url.Values{"category": {"anexo"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("anexo sin nombre: %d", w.Code)
	}
	var docType string
	if err := a.db.QueryRow(`SELECT category,label,doc_type FROM documents WHERE id=?`, v1).Scan(&category, &stored, &docType); err != nil || !strings.HasPrefix(category, "anexo-") || stored != "orden-v1" || docType != "" {
		t.Fatalf("nombre por defecto: %q %q %q %v", category, stored, docType, err)
	}
	if w := do("POST", fmt.Sprintf("/documents/%d/classify", v1), url.Values{"category": {"__new"}, "new_type": {"Garantías"}, "document_label": {"Póliza 1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("tipo nuevo: %d %s", w.Code, w.Body.String())
	}
	if err := a.db.QueryRow(`SELECT label,doc_type FROM documents WHERE id=?`, v1).Scan(&stored, &docType); err != nil || stored != "Póliza 1" || docType != "Garantías" {
		t.Fatalf("tipo nuevo guardado: %q %q %v", stored, docType, err)
	}
	if w := do("POST", fmt.Sprintf("/documents/%d/classify", v1), url.Values{"category": {"t:Garantías"}, "document_label": {"póliza 1"}}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), url.QueryEscape("ya tiene ese tipo")) {
		t.Fatalf("mismo tipo y nombre no debe cambiar nada: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := do("POST", fmt.Sprintf("/documents/%d/classify", v1), url.Values{"category": {"__new"}, "new_type": {" "}}); w.Code != 400 {
		t.Fatalf("tipo nuevo vacío: %d", w.Code)
	}
	// La página muestra el tipo creado como botón y en el selector.
	if page := do("GET", "/process?id=1", nil); !strings.Contains(page.Body.String(), `data-type="Garantías"`) || !strings.Contains(page.Body.String(), `value="t:Garantías"`) {
		t.Fatal("la página debe mostrar el tipo Garantías")
	}
	if w := do("POST", "/documents/999/classify", url.Values{"category": {"orden"}}); w.Code != 404 {
		t.Fatalf("documento inexistente: %d", w.Code)
	}

	// Borrar elimina de verdad el archivo (antes quedaba huérfano en la subcarpeta del tipo).
	if err := a.db.QueryRow(`SELECT category,stored_name FROM documents WHERE id=?`, v2).Scan(&category, &stored); err != nil {
		t.Fatal(err)
	}
	path := a.documentPath(1, category, stored)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if w := do("POST", fmt.Sprintf("/documents/%d/delete", v2), url.Values{"confirm_delete": {"yes"}, "process_id": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("el archivo borrado sigue en disco")
	}
}

func TestUploadDocumentJSON(t *testing.T) {
	a := newTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO processes(id,code,object,created_at) VALUES(1,'RE-1','Compra','2026-11-01 09:00:00')`); err != nil {
		t.Fatal(err)
	}
	post := func(fields map[string]string, filename string, content []byte, json bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		if filename != "" {
			fw, _ := mw.CreateFormFile("document", filename)
			fw.Write(content)
		}
		mw.Close()
		r := httptest.NewRequest("POST", "/processes/documents", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		if json {
			r.Header.Set("Accept", "application/json")
		}
		w := httptest.NewRecorder()
		a.uploadDocument(w, r)
		return w
	}
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("respuesta no es JSON (%d): %q", w.Code, w.Body.String())
		}
		return out
	}

	// Subida correcta: JSON con la versión asignada.
	w := post(map[string]string{"process_id": "1", "category": "terminos"}, "tdr.pdf", []byte("%PDF-1.7 x"), true)
	out := decode(w)
	if w.Code != 200 || out["ok"] != true || out["version"] != float64(1) {
		t.Fatalf("subida: %d %v", w.Code, out)
	}
	w = post(map[string]string{"process_id": "1", "category": "terminos"}, "tdr2.pdf", []byte("%PDF-1.7 y"), true)
	if out = decode(w); out["version"] != float64(2) {
		t.Fatalf("la segunda subida debe ser la versión 2: %v", out)
	}
	// Anexos: el nombre es obligatorio y cada nombre tiene su propio historial.
	w = post(map[string]string{"process_id": "1", "category": "anexo"}, "a.pdf", []byte("%PDF-1.7"), true)
	if out = decode(w); w.Code != 400 || out["ok"] != false || !strings.Contains(fmt.Sprint(out["error"]), "nombre del documento") {
		t.Fatalf("anexo sin nombre: %d %v", w.Code, out)
	}
	w = post(map[string]string{"process_id": "1", "category": "anexo", "document_label": "Acta"}, "a.pdf", []byte("%PDF-1.7"), true)
	if out = decode(w); out["version"] != float64(1) || !strings.Contains(fmt.Sprint(out["message"]), "Acta") {
		t.Fatalf("anexo: %v", out)
	}
	// Archivo vacío, sin archivo y proceso inexistente: error JSON, sin dejar registros.
	if w = post(map[string]string{"process_id": "1", "category": "contrato"}, "vacio.pdf", nil, true); w.Code != 400 || !strings.Contains(fmt.Sprint(decode(w)["error"]), "vacío") {
		t.Fatalf("archivo vacío: %d %s", w.Code, w.Body.String())
	}
	if w = post(map[string]string{"process_id": "1", "category": "contrato"}, "", nil, true); w.Code != 400 {
		t.Fatalf("sin archivo: %d", w.Code)
	}
	if w = post(map[string]string{"process_id": "99", "category": "contrato"}, "x.pdf", []byte("x"), true); w.Code != 404 {
		t.Fatalf("proceso inexistente: %d", w.Code)
	}
	var count int
	a.db.QueryRow(`SELECT count(*) FROM documents`).Scan(&count)
	if count != 3 {
		t.Fatalf("debían quedar 3 documentos, hay %d", count)
	}
	// Sin Accept: JSON se mantiene el comportamiento anterior (redirección y errores en texto).
	if w = post(map[string]string{"process_id": "1", "category": "terminos"}, "tdr3.pdf", []byte("%PDF-1.7 z"), false); w.Code != http.StatusSeeOther {
		t.Fatalf("sin JSON debe redirigir: %d", w.Code)
	}
	if w = post(map[string]string{"process_id": "1", "category": "contrato"}, "", nil, false); w.Code != 400 || strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("error sin JSON: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}
