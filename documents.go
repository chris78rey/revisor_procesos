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

// DocumentGroup reúne todas las versiones de un mismo tipo de documento de un
// proceso. Versions queda ordenado de la versión más nueva a la más antigua.
type DocumentGroup struct {
	Category string
	Title    string
	Rank     int
	Versions []DocumentVersion
	Latest   DocumentVersion
}

// documentCategoryRank fija el orden estándar de los tipos de documento.
func documentCategoryRank(category string) int {
	switch category {
	case "orden":
		return 0
	case "contrato":
		return 1
	case "terminos":
		return 2
	}
	return 3
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

// documentCategoryFromForm valida el tipo elegido en un formulario y devuelve
// la categoría almacenada y su etiqueta. Los anexos se identifican por el
// nombre indicado, de modo que cada anexo conserva su propio historial.
func documentCategoryFromForm(category, label string) (string, string, error) {
	switch category {
	case "orden", "contrato", "terminos":
		return category, "", nil
	case "anexo":
		label = trim(label)
		if label == "" || len(label) > 160 {
			return "", "", errors.New("Indica el nombre del anexo (máximo 160 caracteres)")
		}
		sum := sha256.Sum256([]byte(strings.ToLower(label)))
		return "anexo-" + fmt.Sprintf("%x", sum[:6]), label, nil
	}
	return "", "", errors.New("Proceso o tipo de documento inválido")
}

// groupDocuments agrupa por tipo y ordena: tipos estándar primero y anexos por
// nombre; dentro de cada grupo, la versión más reciente primero.
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
			groups = append(groups, DocumentGroup{Category: doc.Category, Title: doc.Title, Rank: documentCategoryRank(doc.Category)})
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
	category, label, err := documentCategoryFromForm(r.FormValue("category"), r.FormValue("document_label"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var processID int
	var oldCategory, oldStored, filename string
	if err = a.db.QueryRow("SELECT process_id,category,stored_name,filename FROM documents WHERE id=?", id).Scan(&processID, &oldCategory, &oldStored, &filename); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "No se pudo consultar el documento", http.StatusInternalServerError)
		return
	}
	back := "/process?id=" + strconv.Itoa(processID) + "&msg="
	if category == oldCategory {
		http.Redirect(w, r, back+url.QueryEscape("El documento ya tiene ese tipo")+"#documentos", http.StatusSeeOther)
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
	if _, err = a.db.Exec("UPDATE documents SET category=?,label=?,version=?,stored_name=?,signature_report=? WHERE id=?", category, label, version, newStored, string(reviewJSON), id); err != nil {
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
	message := "Documento reclasificado como " + documentCategoryTitle(category, label) + " (versión " + strconv.Itoa(version) + ")"
	http.Redirect(w, r, back+url.QueryEscape(message)+"#documentos", http.StatusSeeOther)
}

func isSignedCategory(category string) bool { return category == "orden" || category == "contrato" }
