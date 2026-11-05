package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DocumentGroup reúne todas las versiones de un mismo documento. Type es el
// tipo con el que se muestra (Contrato, Facturas, Anexos...). Versions queda
// ordenado de la versión más nueva a la más antigua.
type DocumentGroup struct {
	Category string
	Title    string
	Type     string
	Key      string
	Rank     int
	Versions []DocumentVersion
	Latest   DocumentVersion
}

// Tipos de documento propuestos siempre, además de los que el usuario cree.
var presetDocTypes = []string{"Facturas", "Notificaciones", "Informes", "Actas"}

const legacyAnnexType = "Anexos"

func isStandardCategory(category string) bool {
	return category == "orden" || category == "contrato" || category == "terminos"
}

// documentCategoryRank fija el orden de los tipos: primero los tres estándar,
// luego los tipos creados por el usuario y al final los anexos sin tipo.
func documentCategoryRank(category, docType string) int {
	switch category {
	case "orden":
		return 0
	case "contrato":
		return 1
	case "terminos":
		return 2
	}
	if docType != "" {
		return 3
	}
	return 4
}

func documentCategoryTitle(category, label string) string {
	switch {
	case label != "":
		return label
	case category == "orden":
		return "Orden de compra"
	case category == "contrato":
		return "Contrato"
	case category == "terminos":
		return "Términos de referencia"
	}
	return "Anexo"
}

// documentTypeName es el tipo visible de un documento.
func documentTypeName(category, docType string) string {
	switch category {
	case "orden":
		return "Orden de compra"
	case "contrato":
		return "Contrato"
	case "terminos":
		return "Términos de referencia"
	}
	if docType != "" {
		return docType
	}
	return legacyAnnexType
}

func normalizeTypeName(name string) string {
	return strings.Join(strings.Fields(normalizeQueryText(name)), " ")
}

// documentKey identifica un documento dentro de un proceso en la interfaz
// (para calcular el número de la próxima versión).
func documentKey(category, docType, title string) string {
	switch {
	case isStandardCategory(category):
		return category
	case docType != "":
		return "t:" + strings.ToLower(docType) + "|" + strings.ToLower(title)
	}
	return "anexo:" + strings.ToLower(title)
}

// documentSpec es el destino de un documento: categoría (identidad del
// historial de versiones), nombre y tipo visible.
type documentSpec struct {
	Category, Label, DocType string
}

// documentSpecFromForm valida el tipo elegido en un formulario. `category`
// puede ser orden, contrato, terminos, anexo (sin tipo), "t:<Tipo>" (un tipo
// existente) o "__new" (tipo nuevo escrito en newType). Los documentos que no
// son estándar se identifican por tipo y nombre: repetir ambos agrega una
// nueva versión del mismo documento.
func (a *App) documentSpecFromForm(category, label, newType string) (documentSpec, error) {
	docType := ""
	switch {
	case isStandardCategory(category):
		return documentSpec{Category: category}, nil
	case category == "anexo":
	case category == "__new":
		docType = trim(newType)
		if docType == "" {
			return documentSpec{}, errors.New("Escribe el nombre del nuevo tipo")
		}
	case strings.HasPrefix(category, "t:"):
		docType = trim(strings.TrimPrefix(category, "t:"))
	default:
		return documentSpec{}, errors.New("Proceso o tipo de documento inválido")
	}
	if docType != "" {
		if len([]rune(docType)) > 60 {
			return documentSpec{}, errors.New("El nombre del tipo no puede superar 60 caracteres")
		}
		switch normalizeTypeName(docType) {
		case "orden de compra", "orden", "modelo orden de compra", "modelo":
			return documentSpec{Category: "orden"}, nil
		case "contrato", "contratos":
			return documentSpec{Category: "contrato"}, nil
		case "terminos de referencia", "terminos", "especificaciones tecnicas":
			return documentSpec{Category: "terminos"}, nil
		case "anexo", "anexos", "otro anexo", "otros anexos":
			docType = ""
		default:
			docType = a.canonicalDocType(docType)
		}
	}
	label = trim(label)
	if label == "" || len([]rune(label)) > 160 {
		return documentSpec{}, errors.New("Indica el nombre del documento (máximo 160 caracteres)")
	}
	key := strings.ToLower(label)
	if docType != "" {
		key = strings.ToLower(docType) + "|" + key
	}
	sum := sha256.Sum256([]byte(key))
	return documentSpec{Category: "anexo-" + fmt.Sprintf("%x", sum[:6]), Label: label, DocType: docType}, nil
}

// canonicalDocType reutiliza la escritura de un tipo que ya existe
// («facturas» → «Facturas») para no crear tipos duplicados por mayúsculas o tildes.
func (a *App) canonicalDocType(name string) string {
	want := normalizeTypeName(name)
	for _, known := range a.documentTypeOptions() {
		if normalizeTypeName(known) == want {
			return known
		}
	}
	return name
}

// documentTypeOptions lista los tipos propuestos: los predefinidos más todos
// los que se hayan creado en cualquier proceso.
func (a *App) documentTypeOptions() []string {
	seen := map[string]bool{}
	options := []string{}
	add := func(name string) {
		if key := normalizeTypeName(name); key != "" && !seen[key] {
			seen[key] = true
			options = append(options, name)
		}
	}
	for _, name := range presetDocTypes {
		add(name)
	}
	var custom []string
	if rows, err := a.db.Query(`SELECT DISTINCT doc_type FROM documents WHERE doc_type<>''`); err == nil {
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				custom = append(custom, name)
			}
		}
		rows.Close()
	}
	sort.Slice(custom, func(i, j int) bool { return strings.ToLower(custom[i]) < strings.ToLower(custom[j]) })
	for _, name := range custom {
		add(name)
	}
	return options
}

// defaultDocumentLabel propone el nombre del documento a partir del archivo.
func defaultDocumentLabel(filename string) string {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	if r := []rune(name); len(r) > 160 {
		name = string(r[:160])
	}
	return trim(name)
}

// groupDocuments agrupa por documento y ordena: tipos estándar primero, luego
// los tipos creados y los anexos, por nombre; dentro de cada grupo, la versión
// más reciente primero.
func groupDocuments(docs []DocumentVersion) []DocumentGroup {
	index := map[string]int{}
	var groups []DocumentGroup
	for _, doc := range docs {
		doc.Title = documentCategoryTitle(doc.Category, doc.Label)
		doc.Kind = viewableKind(doc.Filename)
		i, ok := index[doc.Category]
		if !ok {
			i = len(groups)
			index[doc.Category] = i
			groups = append(groups, DocumentGroup{
				Category: doc.Category, Title: doc.Title,
				Type: documentTypeName(doc.Category, doc.DocType),
				Key:  documentKey(doc.Category, doc.DocType, doc.Title),
				Rank: documentCategoryRank(doc.Category, doc.DocType),
			})
		}
		groups[i].Versions = append(groups[i].Versions, doc)
	}
	for i := range groups {
		versions := groups[i].Versions
		sort.SliceStable(versions, func(a, b int) bool { return versions[a].Version > versions[b].Version })
		versions[0].Latest = true
		groups[i].Latest = versions[0]
	}
	sort.SliceStable(groups, func(a, b int) bool {
		if groups[a].Rank != groups[b].Rank {
			return groups[a].Rank < groups[b].Rank
		}
		if ta, tb := strings.ToLower(groups[a].Type), strings.ToLower(groups[b].Type); ta != tb {
			return ta < tb
		}
		return strings.ToLower(groups[a].Title) < strings.ToLower(groups[b].Title)
	})
	return groups
}

// viewableKind indica si el visor integrado puede mostrar el archivo.
// Se excluye SVG a propósito: puede contener scripts.
func viewableKind(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		return "pdf"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return "image"
	}
	return ""
}

func viewableContentType(filename string, head []byte) (string, bool) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		if strings.Contains(string(head[:min(len(head), 1024)]), "%PDF-") {
			return "application/pdf", true
		}
	case ".png":
		return "image/png", http.DetectContentType(head) == "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg", http.DetectContentType(head) == "image/jpeg"
	case ".gif":
		return "image/gif", http.DetectContentType(head) == "image/gif"
	case ".webp":
		return "image/webp", http.DetectContentType(head) == "image/webp"
	}
	return "", false
}

func (a *App) documentPath(processID int, category, stored string) string {
	return filepath.Join(a.documentDir, strconv.Itoa(processID), category, filepath.Base(stored))
}

// viewDocument muestra PDFs e imágenes dentro del navegador. El tipo de
// contenido se fija por la extensión y se comprueba contra el contenido real,
// por lo que un archivo renombrado no puede servirse como HTML.
func (a *App) viewDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	var processID int
	var category, stored, filename string
	if err = a.db.QueryRow("SELECT process_id,category,stored_name,filename FROM documents WHERE id=?", id).Scan(&processID, &category, &stored, &filename); err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(a.documentPath(processID, category, stored))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	head := make([]byte, 2048)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	contentType, ok := viewableContentType(filename, head)
	if !ok {
		http.Error(w, "Este tipo de archivo no se puede previsualizar; descárgalo para abrirlo", http.StatusUnsupportedMediaType)
		return
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "No se pudo leer el archivo", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	if strings.HasPrefix(contentType, "image/") {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'self'; sandbox")
	}
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// classifyDocument cambia el tipo de un documento ya cargado. Pasa a ser la
// versión más reciente del tipo de destino y el archivo se mueve a su carpeta.
func (a *App) classifyDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		http.Error(w, "Documento inválido", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	var processID int
	var oldCategory, oldStored, filename string
	if err = a.db.QueryRow("SELECT process_id,category,stored_name,filename FROM documents WHERE id=?", id).Scan(&processID, &oldCategory, &oldStored, &filename); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "No se pudo consultar el documento", http.StatusInternalServerError)
		return
	}
	labelInput := r.FormValue("document_label")
	if trim(labelInput) == "" {
		labelInput = defaultDocumentLabel(filename)
	}
	spec, err := a.documentSpecFromForm(r.FormValue("category"), labelInput, r.FormValue("new_type"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	category, label, docType := spec.Category, spec.Label, spec.DocType
	back := "/process?id=" + strconv.Itoa(processID) + "&msg="
	if category == oldCategory {
		http.Redirect(w, r, back+url.QueryEscape("El documento ya tiene ese tipo y nombre")+"#documentos", http.StatusSeeOther)
		return
	}
	var version int
	if err = a.db.QueryRow("SELECT COALESCE(MAX(version),0)+1 FROM documents WHERE process_id=? AND category=?", processID, category).Scan(&version); err != nil {
		http.Error(w, "No se pudo consultar el historial", http.StatusInternalServerError)
		return
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		http.Error(w, "No se pudo generar el nombre del archivo", http.StatusInternalServerError)
		return
	}
	newStored := fmt.Sprintf("v%d_%x%s", version, nonce[:], strings.ToLower(filepath.Ext(filename)))
	oldPath := a.documentPath(processID, oldCategory, oldStored)
	newPath := a.documentPath(processID, category, newStored)
	if err = os.MkdirAll(filepath.Dir(newPath), 0700); err != nil {
		http.Error(w, "No se pudo preparar el almacenamiento", http.StatusInternalServerError)
		return
	}
	if err = os.Rename(oldPath, newPath); err != nil {
		http.Error(w, "No se pudo mover el archivo", http.StatusInternalServerError)
		return
	}
	review := SignatureReview{}
	if category == "orden" || category == "contrato" {
		review = inspectDocumentSignatures(r.Context(), filename, newPath)
	}
	reviewJSON, _ := json.Marshal(review)
	if _, err = a.db.Exec("UPDATE documents SET category=?,label=?,doc_type=?,version=?,stored_name=?,signature_report=? WHERE id=?", category, label, docType, version, newStored, string(reviewJSON), id); err != nil {
		_ = os.Rename(newPath, oldPath)
		http.Error(w, "No se pudo registrar el cambio de tipo", http.StatusInternalServerError)
		return
	}
	_ = os.Remove(filepath.Dir(oldPath)) // solo se borra si quedó vacía
	if isSignedCategory(oldCategory) || isSignedCategory(category) {
		if err = a.refreshProcessAwardDate(processID); err != nil {
			http.Error(w, "El documento se reclasificó, pero no se pudo actualizar la fecha de adjudicación", http.StatusInternalServerError)
			return
		}
	}
	message := "Documento reclasificado como " + documentTypeName(category, docType)
	if !isStandardCategory(category) {
		message += " · " + label
	}
	message += " (versión " + strconv.Itoa(version) + ")"
	http.Redirect(w, r, back+url.QueryEscape(message)+"#documentos", http.StatusSeeOther)
}

func isSignedCategory(category string) bool { return category == "orden" || category == "contrato" }

// wantsJSON indica que el cliente (la ventana de subida) espera JSON en lugar
// de una redirección o una página de error en texto plano.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func uploadFail(w http.ResponseWriter, r *http.Request, message string, status int) {
	if !wantsJSON(r) {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": message})
}

// adoptLegacyDocumentTypes asigna el tipo a los anexos antiguos cuyo nombre
// coincide con un tipo predefinido (p. ej. un anexo llamado «FACTURAS»).
func adoptLegacyDocumentTypes(db *sql.DB) error {
	rows, err := db.Query(`SELECT id,label FROM documents WHERE doc_type='' AND category LIKE 'anexo-%'`)
	if err != nil {
		return err
	}
	type match struct {
		id      int
		docType string
	}
	var matches []match
	for rows.Next() {
		var id int
		var label string
		if rows.Scan(&id, &label) != nil {
			continue
		}
		for _, preset := range presetDocTypes {
			if normalizeTypeName(label) == normalizeTypeName(preset) {
				matches = append(matches, match{id, preset})
			}
		}
	}
	rows.Close()
	for _, m := range matches {
		if _, err := db.Exec(`UPDATE documents SET doc_type=? WHERE id=?`, m.docType, m.id); err != nil {
			return err
		}
	}
	return nil
}
