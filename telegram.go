package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const telegramPageSize = 8

type telegramAPI struct {
	token  string
	client *http.Client
}

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}

type telegramCallbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message *telegramMessage `json:"message"`
}

type telegramButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

type telegramMarkup struct {
	InlineKeyboard [][]telegramButton `json:"inline_keyboard"`
}

type telegramPendingItem struct {
	ID, ProcessID                                    int
	Code, Object, Document, Summary, Status, DueDate string
}

type telegramObservation struct {
	ID, ProcessID                                                         int
	Code, Object, Document, Summary, Original, Status, DueDate, Assignees string
	Actions                                                               []string
}

type telegramResolutionChallenge struct {
	ObservationID, ProcessID, MessageID, Attempts int
	Year, Code, Input                             string
	CreatedAt                                     int64
}

func parseTelegramAdminIDs(value string) map[int64]struct{} {
	admins := map[int64]struct{}{}
	for _, part := range strings.Split(value, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && id != 0 {
			admins[id] = struct{}{}
		}
	}
	return admins
}

func (a *App) runTelegramBot(ctx context.Context, token string, admins map[int64]struct{}) {
	api := &telegramAPI{token: token, client: &http.Client{Timeout: 40 * time.Second}}
	var me struct {
		Username string `json:"username"`
	}
	if err := api.call(ctx, "getMe", nil, &me); err != nil {
		log.Printf("No se pudo iniciar el bot de Telegram; revise el token y la conexión")
		return
	}
	var webhook struct {
		URL string `json:"url"`
	}
	if err := api.call(ctx, "getWebhookInfo", nil, &webhook); err != nil {
		log.Printf("No se pudo comprobar la configuración del bot de Telegram")
		return
	}
	if webhook.URL != "" {
		log.Printf("El bot @%s ya tiene un webhook configurado; no se inició la lectura por polling", me.Username)
		return
	}
	offset, err := a.telegramUpdateOffset()
	if err != nil {
		log.Printf("No se pudo leer el cursor de Telegram")
		return
	}
	log.Printf("Bot de Telegram @%s activo; mostrará novedades pendientes por chat asignado o administrador", me.Username)
	go a.runTelegramHourlyReminders(ctx, api, admins)
	for ctx.Err() == nil {
		var updates []telegramUpdate
		err := api.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 25, "allowed_updates": []string{"message", "callback_query"},
		}, &updates)
		if err != nil {
			log.Printf("No se pudieron consultar novedades de Telegram; se reintentará")
			if !waitContext(ctx, 3*time.Second) {
				return
			}
			continue
		}
		for _, update := range updates {
			if err := a.handleTelegramUpdate(ctx, api, admins, update); err != nil {
				log.Printf("No se pudo procesar interacción de Telegram (update %d): %v", update.UpdateID, err)
			}
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
				if err := a.saveTelegramUpdateOffset(offset); err != nil {
					log.Printf("No se pudo guardar el cursor de Telegram")
				}
			}
		}
	}
}

type telegramReminderProcess struct {
	ID, OpenCount int
	Code, Object  string
}

func (a *App) runTelegramHourlyReminders(ctx context.Context, api *telegramAPI, admins map[int64]struct{}) {
	location, err := time.LoadLocation("America/Guayaquil")
	if err != nil {
		location = time.Local
	}
	for ctx.Err() == nil {
		now := time.Now().In(location)
		next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, location)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := a.sendTelegramHourlyReminders(ctx, api, admins, time.Now().In(location)); err != nil {
				log.Printf("No se pudo preparar el recordatorio horario de Telegram; se intentará en la próxima hora")
			}
		}
	}
}

func (a *App) sendTelegramHourlyReminders(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, now time.Time) error {
	if hour := now.Hour(); hour < 8 || hour > 18 {
		return nil
	}
	year := strconv.Itoa(now.Year())
	hourKey := now.Format("2006-01-02-15")
	rows, err := a.db.Query(`SELECT DISTINCT chat_id FROM telegram_contacts WHERE chat_id<>''`)
	if err != nil {
		return err
	}
	recipients := map[int64]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && id > 0 {
			recipients[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for id := range admins {
		if id > 0 {
			recipients[id] = struct{}{}
		}
	}
	for chatID := range recipients {
		var alreadySent int
		err := a.db.QueryRow(`SELECT count(*) FROM telegram_notification_runs WHERE chat_id=? AND hour_key=?`, strconv.FormatInt(chatID, 10), hourKey).Scan(&alreadySent)
		if err != nil {
			return err
		}
		if alreadySent > 0 {
			continue
		}
		processRows, err := a.db.Query(`SELECT p.id,p.code,p.object,count(DISTINCT o.id) FROM processes p JOIN observations o ON o.process_id=p.id
			WHERE p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
			AND (substr(p.accounting_date,1,4)=? OR substr(o.received_date,1,4)=? OR substr(o.created_at,1,4)=?)
			GROUP BY p.id ORDER BY p.id DESC`, year, year, year)
		if err != nil {
			return err
		}
		var processes []telegramReminderProcess
		for processRows.Next() {
			var p telegramReminderProcess
			if err := processRows.Scan(&p.ID, &p.Code, &p.Object, &p.OpenCount); err != nil {
				processRows.Close()
				return err
			}
			processes = append(processes, p)
		}
		if err := processRows.Err(); err != nil {
			processRows.Close()
			return err
		}
		if err := processRows.Close(); err != nil {
			return err
		}
		if len(processes) == 0 {
			continue
		}
		text := fmt.Sprintf("⏰ Recordatorio de revisión · %s\nHay %d proceso(s) con novedades pendientes:", year, len(processes))
		perItem := (3700 - len([]rune(text))) / len(processes)
		if perItem > 260 {
			perItem = 260
		}
		if perItem < 24 {
			perItem = 24
		}
		keyboard := make([][]telegramButton, 0, len(processes)+1)
		for i, p := range processes {
			code := p.Code
			if code == "" {
				code = fmt.Sprintf("Proceso #%d", p.ID)
			}
			line := fmt.Sprintf("\n%d. %s — %s (%d nov.)", i+1, code, p.Object, p.OpenCount)
			text += telegramShort(line, perItem)
			keyboard = append(keyboard, []telegramButton{{Text: telegramShort("Revisar "+code, 58), CallbackData: fmt.Sprintf("p:%s:%d", year, p.ID)}})
		}
		keyboard = append(keyboard, []telegramButton{{Text: "Abrir todas las novedades", CallbackData: "yr:" + year}})
		if err := api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": telegramShort(text, 3900), "reply_markup": telegramMarkup{InlineKeyboard: keyboard}}, nil); err != nil {
			log.Printf("No se pudo enviar un recordatorio horario de Telegram; se omitirá este chat en este ciclo")
			continue
		}
		if _, err := a.db.Exec(`INSERT OR IGNORE INTO telegram_notification_runs(chat_id,hour_key,sent_at) VALUES(?,?,?)`, strconv.FormatInt(chatID, 10), hourKey, now.Format(time.RFC3339)); err != nil {
			return err
		}
		if !waitContext(ctx, 50*time.Millisecond) {
			return nil
		}
	}
	return nil
}

func (api *telegramAPI) call(ctx context.Context, method string, payload, result any) error {
	if method == "sendMessage" {
		if values, ok := payload.(map[string]any); ok {
			if text, isText := values["text"].(string); !isText || text != "Por seguridad, usa este bot en un chat privado." {
				markup, _ := values["reply_markup"].(telegramMarkup)
				values["reply_markup"] = telegramMarkupWithMenu(markup)
			}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if payload == nil {
		body = []byte("{}")
	}
	endpoint := "https://api.telegram.org/bot" + api.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("no se pudo preparar la solicitud de Telegram")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := api.client.Do(req)
	if err != nil {
		return errors.New("Telegram no está disponible")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return errors.New("no se pudo leer la respuesta de Telegram")
	}
	var envelope telegramResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return errors.New("respuesta inválida de Telegram")
	}
	if !envelope.OK {
		return fmt.Errorf("Telegram respondió: %s", envelope.Description)
	}
	if result != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return errors.New("resultado inválido de Telegram")
		}
	}
	return nil
}

func (api *telegramAPI) sendDocumentFile(ctx context.Context, chatID int64, path, filename, caption string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("no se encontró el archivo de documentación")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return errors.New("no se pudo leer el archivo de documentación")
	}
	if !info.Mode().IsRegular() || info.Size() > 50<<20 {
		return errors.New("el archivo supera el límite de envío de Telegram")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err = writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if caption != "" {
		if err = writer.WriteField("caption", telegramShort(caption, 900)); err != nil {
			return err
		}
	}
	markupJSON, err := json.Marshal(telegramMarkupWithMenu(telegramMarkup{}))
	if err != nil {
		return err
	}
	if err = writer.WriteField("reply_markup", string(markupJSON)); err != nil {
		return err
	}
	part, err := writer.CreateFormFile("document", filepath.Base(filename))
	if err != nil {
		return err
	}
	if _, err = io.Copy(part, file); err != nil {
		return errors.New("no se pudo preparar el archivo para Telegram")
	}
	if err = writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+api.token+"/sendDocument", &body)
	if err != nil {
		return errors.New("no se pudo preparar la descarga")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := api.client.Do(req)
	if err != nil {
		return errors.New("Telegram no está disponible para enviar el archivo")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return errors.New("no se pudo leer la respuesta de Telegram")
	}
	var result telegramResponse
	if err = json.Unmarshal(data, &result); err != nil {
		return errors.New("respuesta inválida de Telegram")
	}
	if !result.OK {
		return fmt.Errorf("Telegram rechazó el archivo: %s", result.Description)
	}
	return nil
}

func (a *App) handleTelegramUpdate(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, update telegramUpdate) error {
	if update.CallbackQuery != nil {
		return a.handleTelegramCallback(ctx, api, admins, update.CallbackQuery)
	}
	message := update.Message
	if message == nil || message.Text == "" {
		return nil
	}
	if message.Chat.Type != "private" {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": message.Chat.ID, "text": "Por seguridad, usa este bot en un chat privado."}, nil)
	}
	fields := strings.Fields(message.Text)
	if len(fields) == 0 {
		return nil
	}
	command := fields[0]
	command = strings.SplitN(command, "@", 2)[0]
	admin := isTelegramAdmin(message.Chat.ID, admins)
	challenge, challengeErr := a.telegramResolutionChallenge(message.Chat.ID)
	if challengeErr != nil && !errors.Is(challengeErr, sql.ErrNoRows) {
		return challengeErr
	}
	if strings.SplitN(fields[0], "@", 2)[0] == "/start" && len(fields) == 2 {
		return a.redeemTelegramLinkInvite(ctx, api, message.Chat.ID, fields[1])
	}
	if admin {
		var pendingName string
		pendingErr := a.db.QueryRow(`SELECT name FROM telegram_manual_link_pending WHERE chat_id=?`, strconv.FormatInt(message.Chat.ID, 10)).Scan(&pendingName)
		if pendingErr == nil {
			idText := strings.TrimSpace(message.Text)
			id, parseErr := strconv.ParseInt(idText, 10, 64)
			if parseErr != nil || id <= 0 || strings.HasPrefix(idText, "+") {
				return api.call(ctx, "sendMessage", map[string]any{"chat_id": message.Chat.ID, "text": "Ese ID no parece válido. Envía solo el número que la persona obtiene con /id, o pulsa 🏠 para cancelar."}, nil)
			}
			if _, err := a.db.Exec(`INSERT INTO telegram_contacts(name,chat_id,updated_at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET chat_id=excluded.chat_id,updated_at=excluded.updated_at`, pendingName, strconv.FormatInt(id, 10), now()); err != nil {
				return err
			}
			if _, err := a.db.Exec(`DELETE FROM telegram_manual_link_pending WHERE chat_id=?`, strconv.FormatInt(message.Chat.ID, 10)); err != nil {
				return err
			}
			return api.call(ctx, "sendMessage", map[string]any{"chat_id": message.Chat.ID, "text": fmt.Sprintf("✅ %s quedó vinculado al ID de Telegram %d.", pendingName, id)}, nil)
		}
		if !errors.Is(pendingErr, sql.ErrNoRows) {
			return pendingErr
		}
	}
	if challengeErr == nil {
		if time.Now().Unix()-challenge.CreatedAt > 300 {
			if err := a.clearTelegramResolutionChallenge(message.Chat.ID); err != nil {
				return err
			}
			return api.editMessage(ctx, message.Chat.ID, challenge.MessageID, "El código venció. La novedad sigue pendiente; vuelve a iniciar la confirmación.", telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "Ver detalle", CallbackData: telegramNoveltyCallback("v", challenge.ObservationID, 0, challenge.Year, challenge.ProcessID)}}}})
		}
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": message.Chat.ID, "text": "Para confirmar, toca las cifras en los botones del mensaje del bot. No escribas el código."}, nil)
	}
	switch command {
	case "/start", "/menu":
		if admin {
			_, _ = a.db.Exec(`DELETE FROM telegram_manual_link_pending WHERE chat_id=?`, strconv.FormatInt(message.Chat.ID, 10))
		}
		return a.sendTelegramMenu(ctx, api, admins, message.Chat.ID, nil)
	case "/id":
		return api.call(ctx, "sendMessage", map[string]any{
			"chat_id": message.Chat.ID,
			"text":    fmt.Sprintf("Tu ID numérico de Telegram es %d. Compártelo con el administrador para que pueda vincularte como responsable.", message.Chat.ID),
		}, nil)
	case "/pendientes":
		return a.sendTelegramYears(ctx, api, admins, message.Chat.ID, nil)
	case "/usuarios":
		if !admin {
			return api.call(ctx, "sendMessage", map[string]any{"chat_id": message.Chat.ID, "text": "Esta opción es solo para administradores."}, nil)
		}
		return a.sendTelegramUsers(ctx, api, admins, message.Chat.ID, 0, nil)
	default:
		return nil
	}
}

func (a *App) handleTelegramCallback(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, query *telegramCallbackQuery) error {
	if query.Message == nil {
		return api.answerCallback(ctx, query.ID, "Abre el bot en un chat privado.", true)
	}
	chatID := query.Message.Chat.ID
	if query.Message.Chat.Type != "private" {
		return api.answerCallback(ctx, query.ID, "Usa el bot en un chat privado.", true)
	}
	parts := strings.Split(query.Data, ":")
	if len(parts) < 2 {
		return api.answerCallback(ctx, query.ID, "Acción no válida.", true)
	}
	action := parts[0]
	switch action {
	case "m":
		if err := a.clearTelegramResolutionChallenge(chatID); err != nil {
			return err
		}
		if isTelegramAdmin(chatID, admins) {
			if _, err := a.db.Exec(`DELETE FROM telegram_manual_link_pending WHERE chat_id=?`, strconv.FormatInt(chatID, 10)); err != nil {
				return err
			}
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramMenu(ctx, api, admins, chatID, query.Message)
	case "dg":
		if len(parts) != 2 || len(parts[1]) != 1 || parts[1][0] < '0' || parts[1][0] > '9' {
			return api.answerCallback(ctx, query.ID, "Cifra no válida.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.handleTelegramResolutionDigit(ctx, api, admins, chatID, query.Message, parts[1])
	case "u":
		if !isTelegramAdmin(chatID, admins) {
			return api.answerCallback(ctx, query.ID, "Esta opción es solo para administradores.", true)
		}
		offset, _ := strconv.Atoi(parts[1])
		if offset < 0 {
			offset = 0
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramUsers(ctx, api, admins, chatID, offset, query.Message)
	case "ua":
		if !isTelegramAdmin(chatID, admins) {
			return api.answerCallback(ctx, query.ID, "Esta opción es solo para administradores.", true)
		}
		offset, _ := strconv.Atoi(parts[1])
		if offset < 0 {
			offset = 0
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramLinkableUsers(ctx, api, admins, chatID, offset, query.Message)
	case "ul":
		if !isTelegramAdmin(chatID, admins) {
			return api.answerCallback(ctx, query.ID, "Esta opción es solo para administradores.", true)
		}
		if len(parts) != 3 {
			return api.answerCallback(ctx, query.ID, "Selección no válida.", true)
		}
		offset, e1 := strconv.Atoi(parts[1])
		index, e2 := strconv.Atoi(parts[2])
		if e1 != nil || e2 != nil || offset < 0 || index < 0 || index >= 10 {
			return api.answerCallback(ctx, query.ID, "Selección no válida.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.createTelegramLinkInvite(ctx, api, chatID, query.Message, offset, index)
	case "uc":
		if !isTelegramAdmin(chatID, admins) {
			return api.answerCallback(ctx, query.ID, "Esta opción es solo para administradores.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "Registro cancelado.", false); err != nil {
			return err
		}
		return a.sendTelegramUsers(ctx, api, admins, chatID, 0, query.Message)
	case "y":
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramYears(ctx, api, admins, chatID, query.Message)
	case "yr":
		if len(parts) != 2 || !validTelegramYear(parts[1]) {
			return api.answerCallback(ctx, query.ID, "Año no válido.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramProcesses(ctx, api, admins, chatID, parts[1], 0, query.Message)
	case "pg":
		if len(parts) != 3 || !validTelegramYear(parts[1]) {
			return api.answerCallback(ctx, query.ID, "Proceso no válido.", true)
		}
		offset, err := strconv.Atoi(parts[2])
		if err != nil || offset < 0 {
			return api.answerCallback(ctx, query.ID, "Página no válida.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramProcesses(ctx, api, admins, chatID, parts[1], offset, query.Message)
	case "p":
		if len(parts) != 3 || !validTelegramYear(parts[1]) {
			return api.answerCallback(ctx, query.ID, "Proceso no válido.", true)
		}
		processID, err := strconv.Atoi(parts[2])
		if err != nil || processID < 1 {
			return api.answerCallback(ctx, query.ID, "Proceso no válido.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramProcessNovelties(ctx, api, admins, chatID, parts[1], processID, 0, query.Message)
	case "n":
		if (len(parts) != 3 && len(parts) != 4) || !validTelegramYear(parts[1]) {
			return api.answerCallback(ctx, query.ID, "Novedad no válida.", true)
		}
		processID, e1 := strconv.Atoi(parts[2])
		offset, e2 := 0, error(nil)
		if len(parts) == 4 {
			offset, e2 = strconv.Atoi(parts[3])
		}
		if e1 != nil || e2 != nil || processID < 1 || offset < 0 {
			return api.answerCallback(ctx, query.ID, "Navegación no válida.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramProcessNovelties(ctx, api, admins, chatID, parts[1], processID, offset, query.Message)
	case "d":
		if len(parts) != 4 || !validTelegramYear(parts[2]) {
			return api.answerCallback(ctx, query.ID, "Documento no válido.", true)
		}
		docID, e1 := strconv.Atoi(parts[1])
		processID, e2 := strconv.Atoi(parts[3])
		if e1 != nil || e2 != nil || docID < 1 || processID < 1 {
			return api.answerCallback(ctx, query.ID, "Documento no válido.", true)
		}
		if err := api.answerCallback(ctx, query.ID, "Preparando descarga…", false); err != nil {
			return err
		}
		return a.sendTelegramProcessDocument(ctx, api, admins, chatID, docID, parts[2], processID)
	case "l":
		offset, _ := strconv.Atoi(parts[1])
		if offset < 0 {
			offset = 0
		}
		if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
			return err
		}
		return a.sendTelegramPending(ctx, api, admins, chatID, offset, query.Message)
	case "v", "r", "c", "x", "b":
		if len(parts) != 3 && len(parts) != 5 {
			return api.answerCallback(ctx, query.ID, "Acción no válida.", true)
		}
		observationID, err := strconv.Atoi(parts[1])
		if err != nil || observationID < 1 {
			return api.answerCallback(ctx, query.ID, "Novedad no válida.", true)
		}
		offset, _ := strconv.Atoi(parts[2])
		if offset < 0 {
			offset = 0
		}
		year, processID := "all", 0
		if len(parts) == 5 {
			year = parts[3]
			processID, err = strconv.Atoi(parts[4])
			if !validTelegramYear(year) || err != nil || processID < 1 {
				return api.answerCallback(ctx, query.ID, "Navegación no válida.", true)
			}
		}
		var allowed bool
		if action == "r" || action == "c" || action == "x" {
			allowed, err = a.telegramCanAccess(chatID, observationID, admins)
		} else {
			allowed, err = a.telegramCanViewObservation(chatID, observationID, admins)
		}
		if err != nil {
			return err
		}
		if !allowed {
			return api.answerCallback(ctx, query.ID, "No tienes acceso a esta novedad.", true)
		}
		switch action {
		case "b":
			if processID < 1 {
				return a.sendTelegramYears(ctx, api, admins, chatID, query.Message)
			}
			if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
				return err
			}
			return a.sendTelegramProcessNovelties(ctx, api, admins, chatID, year, processID, offset, query.Message)
		case "v":
			if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
				return err
			}
			return a.editTelegramObservation(ctx, api, admins, query.Message, observationID, offset, false, year, processID)
		case "r":
			if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
				return err
			}
			return a.editTelegramObservation(ctx, api, admins, query.Message, observationID, offset, true, year, processID)
		case "x":
			if err := a.clearTelegramResolutionChallenge(chatID); err != nil {
				return err
			}
			if err := api.answerCallback(ctx, query.ID, "Cancelado. La novedad sigue pendiente.", false); err != nil {
				return err
			}
			return a.editTelegramObservation(ctx, api, admins, query.Message, observationID, offset, false, year, processID)
		case "c":
			if err := api.answerCallback(ctx, query.ID, "", false); err != nil {
				return err
			}
			return a.startTelegramResolutionChallenge(ctx, api, chatID, query.Message, observationID, year, processID)
		}
	}
	return api.answerCallback(ctx, query.ID, "Acción no disponible.", true)
}

func validTelegramYear(year string) bool {
	if year == "all" {
		return true
	}
	if len(year) != 4 {
		return false
	}
	_, err := strconv.Atoi(year)
	return err == nil
}

func (a *App) sendTelegramMenu(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, existing *telegramMessage) error {
	keyboard := [][]telegramButton{{{Text: "Ver novedades pendientes", CallbackData: "y:0"}}}
	text := "Menú principal\n\nElige una opción. Puedes volver aquí desde cualquier pantalla con 🏠."
	if isTelegramAdmin(chatID, admins) {
		keyboard = append(keyboard, []telegramButton{{Text: "➕ Agregar responsable", CallbackData: "ua:0"}})
		keyboard = append(keyboard, []telegramButton{{Text: "Responsables vinculados", CallbackData: "u:0"}})
	}
	markup := telegramMarkup{InlineKeyboard: keyboard}
	if existing != nil {
		return api.editMessage(ctx, chatID, existing.MessageID, text, markup)
	}
	return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "reply_markup": markup}, nil)
}

func (a *App) telegramAccessArgs(chatID int64, admins map[int64]struct{}) (int, string) {
	return boolInt(isTelegramAdmin(chatID, admins)), strconv.FormatInt(chatID, 10)
}

func (a *App) sendTelegramYears(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, existing *telegramMessage) error {
	adminFlag, chat := a.telegramAccessArgs(chatID, admins)
	rows, err := a.db.Query(`SELECT yr FROM (
		SELECT substr(p.accounting_date,1,4) yr FROM observations o JOIN processes p ON p.id=o.process_id
		WHERE p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?))
		UNION SELECT substr(o.received_date,1,4) FROM observations o JOIN processes p ON p.id=o.process_id
		WHERE p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?))
		UNION SELECT substr(o.created_at,1,4) FROM observations o JOIN processes p ON p.id=o.process_id
		WHERE p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?))
	) WHERE yr<>'' ORDER BY yr DESC`, adminFlag, chat, adminFlag, chat, adminFlag, chat)
	if err != nil {
		return err
	}
	var years []string
	for rows.Next() {
		var year string
		if err := rows.Scan(&year); err != nil {
			rows.Close()
			return err
		}
		years = append(years, year)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	text := "Elige el año para ver las novedades abiertas de todos los procesos:"
	keyboard := make([][]telegramButton, 0, len(years)+1)
	for _, year := range years {
		keyboard = append(keyboard, []telegramButton{{Text: year, CallbackData: "yr:" + year}})
	}
	keyboard = append(keyboard, []telegramButton{{Text: "Todos los años", CallbackData: "yr:all"}})
	if len(years) == 0 {
		text += "\n\nNo hay novedades abiertas disponibles."
	}
	markup := telegramMarkup{InlineKeyboard: keyboard}
	if existing != nil {
		return api.editMessage(ctx, chatID, existing.MessageID, text, markup)
	}
	return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "reply_markup": markup}, nil)
}

func (a *App) sendTelegramProcesses(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, year string, offset int, existing *telegramMessage) error {
	adminFlag, chat := a.telegramAccessArgs(chatID, admins)
	rows, err := a.db.Query(`SELECT DISTINCT p.id,p.code,p.object FROM processes p JOIN observations o ON o.process_id=p.id
		WHERE p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
		AND (?='all' OR substr(p.accounting_date,1,4)=? OR substr(o.received_date,1,4)=? OR substr(o.created_at,1,4)=?)
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?))
		ORDER BY p.id DESC LIMIT ? OFFSET ?`, year, year, year, year, adminFlag, chat, 6, offset)
	if err != nil {
		return err
	}
	type process struct {
		id           int
		code, object string
	}
	var items []process
	for rows.Next() {
		var p process
		if err := rows.Scan(&p.id, &p.code, &p.object); err != nil {
			rows.Close()
			return err
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	hasMore := len(items) > 5
	if hasMore {
		items = items[:5]
	}
	text := "Procesos con novedades pendientes"
	if year != "all" {
		text += " · " + year
	}
	keyboard := make([][]telegramButton, 0, len(items)+2)
	for i, p := range items {
		object := strings.TrimSpace(p.object)
		if object == "" {
			object = fmt.Sprintf("Proceso #%d", p.id)
		}
		code := p.code
		if code == "" {
			code = fmt.Sprintf("#%d", p.id)
		}
		text += fmt.Sprintf("\n\n%d. %s\nCódigo: %s", offset+i+1, object, code)
		keyboard = append(keyboard, []telegramButton{{Text: telegramShort("Ver: "+object, 58), CallbackData: fmt.Sprintf("p:%s:%d", year, p.id)}})
	}
	if len(items) == 0 {
		text += "\n\nNo hay procesos con novedades abiertas en este año."
	}
	if hasMore || offset > 0 {
		var nav []telegramButton
		if offset > 0 {
			prev := offset - 5
			if prev < 0 {
				prev = 0
			}
			nav = append(nav, telegramButton{Text: "← Anterior", CallbackData: fmt.Sprintf("pg:%s:%d", year, prev)})
		}
		if hasMore {
			nav = append(nav, telegramButton{Text: "Siguiente →", CallbackData: fmt.Sprintf("pg:%s:%d", year, offset+5)})
		}
		keyboard = append(keyboard, nav)
	}
	keyboard = append(keyboard, []telegramButton{{Text: "← Elegir año", CallbackData: "y:0"}})
	return api.editMessage(ctx, chatID, existing.MessageID, text, telegramMarkup{InlineKeyboard: keyboard})
}

func (a *App) sendTelegramProcessNovelties(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, year string, processID, offset int, existing *telegramMessage) error {
	_ = offset // conservado para callbacks antiguos; el listado nuevo siempre es una sola página.
	adminFlag, chat := a.telegramAccessArgs(chatID, admins)
	var object, code string
	err := a.db.QueryRow(`SELECT p.object,p.code FROM processes p WHERE p.id=? AND p.status<>'Archivado' AND EXISTS(
		SELECT 1 FROM observations o WHERE o.process_id=p.id AND o.status NOT IN ('Subsanada','No aplica')
		AND (?='all' OR substr(p.accounting_date,1,4)=? OR substr(o.received_date,1,4)=? OR substr(o.created_at,1,4)=?)
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?)))`, processID, year, year, year, year, adminFlag, chat).Scan(&object, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return api.editMessage(ctx, chatID, existing.MessageID, "El proceso no tiene novedades disponibles o no tienes acceso.", telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "← Elegir año", CallbackData: "y:0"}}}})
	}
	if err != nil {
		return err
	}
	rows, err := a.db.Query(`SELECT o.id,o.document,o.summary,o.status,o.due_date,o.original,o.area,o.received_date,o.invoice_amount,
		COALESCE((SELECT group_concat(a.name,', ') FROM assignees a WHERE a.observation_id=o.id),'')
		FROM observations o JOIN processes p ON p.id=o.process_id WHERE p.id=? AND p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
		AND (?='all' OR substr(p.accounting_date,1,4)=? OR substr(o.received_date,1,4)=? OR substr(o.created_at,1,4)=?)
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?)) ORDER BY CASE WHEN o.due_date='' THEN 1 ELSE 0 END,o.due_date,o.id DESC`, processID, year, year, year, year, adminFlag, chat)
	if err != nil {
		return err
	}
	type novelty struct {
		id                                          int
		document, summary, status, due              string
		original, area, received, amount, assignees string
	}
	var items []novelty
	for rows.Next() {
		var n novelty
		if err := rows.Scan(&n.id, &n.document, &n.summary, &n.status, &n.due, &n.original, &n.area, &n.received, &n.amount, &n.assignees); err != nil {
			rows.Close()
			return err
		}
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if code == "" {
		code = fmt.Sprintf("Proceso #%d", processID)
	}
	if strings.TrimSpace(object) == "" {
		object = code
	}
	text := fmt.Sprintf("📂 %s\n%s\n\nNovedades pendientes:", code, object)
	keyboard := make([][]telegramButton, 0, len(items)+2)
	perItem := 350
	if len(items) > 0 {
		perItem = (3700 - len([]rune(text))) / len(items)
		if perItem > 650 {
			perItem = 650
		}
		if perItem < 50 {
			perItem = 50
		}
	}
	for i, n := range items {
		doc := n.document
		if doc == "" {
			doc = strings.TrimSpace(n.summary)
		}
		if doc == "" {
			doc = "Novedad sin título"
		}
		detail := strings.TrimSpace(n.original)
		if detail == "" {
			detail = strings.TrimSpace(n.summary)
		}
		prefix := fmt.Sprintf("\n\n%d. %s", i+1, telegramShort(doc, 90))
		meta := fmt.Sprintf("\nEstado: %s · Límite: %s", n.status, telegramOrDash(n.due))
		if n.assignees != "" {
			meta += " · Responsable: " + telegramShort(n.assignees, 75)
		}
		detailLimit := perItem - len([]rune(prefix)) - len([]rune(meta)) - 3
		if detailLimit < 12 {
			detailLimit = 12
		}
		text += prefix + "\n" + telegramShort(telegramOrDash(detail), detailLimit) + meta
		buttonText := "Abrir y revisar: " + doc
		if n.document == "" {
			buttonText = "Abrir detalle de novedad " + strconv.Itoa(offset+i+1)
		}
		keyboard = append(keyboard, []telegramButton{{Text: telegramShort(buttonText, 58), CallbackData: fmt.Sprintf("v:%d:%d:%s:%d", n.id, offset, year, processID)}})
	}
	if len(items) == 0 {
		text += "\n\nNo quedan novedades abiertas."
	}
	docs, err := a.db.Query(`SELECT id,category,label,filename,version FROM documents WHERE process_id=? ORDER BY category,label,version DESC`, processID)
	if err != nil {
		return err
	}
	type fileItem struct {
		id, version               int
		category, label, filename string
	}
	var files []fileItem
	for docs.Next() {
		var f fileItem
		if err := docs.Scan(&f.id, &f.category, &f.label, &f.filename, &f.version); err != nil {
			docs.Close()
			return err
		}
		files = append(files, f)
	}
	if err := docs.Err(); err != nil {
		docs.Close()
		return err
	}
	if err := docs.Close(); err != nil {
		return err
	}
	if len(files) > 0 {
		text += "\n\nDocumentación descargable:"
	}
	for _, f := range files {
		label := strings.TrimSpace(f.label)
		if label == "" {
			label = f.filename
		}
		if label == "" {
			label = f.category
		}
		text += fmt.Sprintf("\n📎 %s · v%d", telegramShort(label, 90), f.version)
		keyboard = append(keyboard, []telegramButton{{Text: telegramShort("⬇ Descargar: "+label, 58), CallbackData: fmt.Sprintf("d:%d:%s:%d", f.id, year, processID)}})
	}
	keyboard = append(keyboard, []telegramButton{{Text: "← Procesos", CallbackData: "yr:" + year}})
	return api.editMessage(ctx, chatID, existing.MessageID, telegramShort(text, 3900), telegramMarkup{InlineKeyboard: keyboard})
}

func (a *App) sendTelegramPending(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, offset int, existing *telegramMessage) error {
	admin := isTelegramAdmin(chatID, admins)
	rows, err := a.db.Query(`SELECT o.id,p.id,p.code,p.object,o.document,o.summary,o.status,o.due_date
		FROM observations o JOIN processes p ON p.id=o.process_id
		WHERE p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica')
		AND (?=1 OR EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?))
		ORDER BY CASE WHEN o.due_date='' THEN 1 ELSE 0 END,o.due_date,o.id DESC LIMIT ? OFFSET ?`, boolInt(admin), strconv.FormatInt(chatID, 10), telegramPageSize+1, offset)
	if err != nil {
		return err
	}
	items := make([]telegramPendingItem, 0, telegramPageSize+1)
	for rows.Next() {
		var item telegramPendingItem
		if err := rows.Scan(&item.ID, &item.ProcessID, &item.Code, &item.Object, &item.Document, &item.Summary, &item.Status, &item.DueDate); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	hasMore := len(items) > telegramPageSize
	if hasMore {
		items = items[:telegramPageSize]
	}
	text := "Novedades pendientes de solventar"
	if !admin {
		text += " disponibles para revisar"
	}
	keyboard := make([][]telegramButton, 0, len(items)+1)
	for _, item := range items {
		code := item.Code
		if code == "" {
			code = fmt.Sprintf("Proceso #%d", item.ProcessID)
		}
		detail := item.Document
		if detail == "" {
			detail = item.Summary
		}
		if detail == "" {
			detail = "Ver novedad"
		}
		keyboard = append(keyboard, []telegramButton{{Text: telegramShort(code+" · "+detail, 60), CallbackData: fmt.Sprintf("v:%d:%d", item.ID, offset)}})
	}
	if hasMore || offset > 0 {
		var page []telegramButton
		if offset > 0 {
			prev := offset - telegramPageSize
			if prev < 0 {
				prev = 0
			}
			page = append(page, telegramButton{Text: "← Anterior", CallbackData: fmt.Sprintf("l:%d", prev)})
		}
		if hasMore {
			page = append(page, telegramButton{Text: "Siguiente →", CallbackData: fmt.Sprintf("l:%d", offset+telegramPageSize)})
		}
		keyboard = append(keyboard, page)
	}
	if len(items) == 0 {
		if !admin {
			var count int
			_ = a.db.QueryRow(`SELECT count(*) FROM telegram_contacts WHERE chat_id=?`, strconv.FormatInt(chatID, 10)).Scan(&count)
			if count == 0 {
				text += "\n\nEste chat aún no está vinculado a un responsable. Solicita al administrador un enlace de invitación y ábrelo para vincularte automáticamente."
			} else {
				text += "\n\nNo hay novedades abiertas."
			}
		} else {
			text += "\n\nNo hay novedades abiertas."
		}
	}
	markup := telegramMarkup{InlineKeyboard: keyboard}
	if existing != nil {
		return api.editMessage(ctx, existing.Chat.ID, existing.MessageID, text, markup)
	}
	return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "reply_markup": markup}, nil)
}

func (a *App) sendTelegramUsers(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, offset int, existing *telegramMessage) error {
	if !isTelegramAdmin(chatID, admins) {
		if existing != nil {
			return api.editMessage(ctx, chatID, existing.MessageID, "Esta opción es solo para administradores.", telegramMarkup{})
		}
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "Esta opción es solo para administradores."}, nil)
	}
	rows, err := a.db.Query("SELECT name,chat_id FROM telegram_contacts ORDER BY name LIMIT ? OFFSET ?", 16, offset)
	if err != nil {
		return err
	}
	var lines []string
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, "• "+name+" — "+id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	text := "Usuarios vinculados a Telegram"
	if len(lines) > 0 {
		text += "\n\n" + strings.Join(lines, "\n")
	} else {
		text += "\n\nAún no hay usuarios vinculados."
	}
	text += "\n\nAl asignar una novedad, usa exactamente el nombre vinculado."
	keyboard := [][]telegramButton{{{Text: "➕ Agregar responsable", CallbackData: "ua:0"}}}
	if offset > 0 || len(lines) == 16 {
		var page []telegramButton
		if offset > 0 {
			prev := offset - 16
			if prev < 0 {
				prev = 0
			}
			page = append(page, telegramButton{Text: "← Anterior", CallbackData: fmt.Sprintf("u:%d", prev)})
		}
		if len(lines) == 16 {
			page = append(page, telegramButton{Text: "Siguiente →", CallbackData: fmt.Sprintf("u:%d", offset+16)})
		}
		keyboard = append(keyboard, page)
	}
	keyboard = append(keyboard, []telegramButton{{Text: "Ver novedades pendientes", CallbackData: "y:0"}})
	markup := telegramMarkup{InlineKeyboard: keyboard}
	if existing != nil {
		return api.editMessage(ctx, chatID, existing.MessageID, text, markup)
	}
	return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "reply_markup": markup}, nil)
}

func (a *App) sendTelegramLinkableUsers(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, offset int, existing *telegramMessage) error {
	if !isTelegramAdmin(chatID, admins) {
		return api.editMessage(ctx, chatID, existing.MessageID, "Esta opción es solo para administradores.", telegramMarkup{})
	}
	rows, err := a.db.Query(`SELECT a.name,COALESCE((SELECT t.chat_id FROM telegram_contacts t WHERE lower(t.name)=lower(a.name) LIMIT 1),'')
		FROM assignees a GROUP BY lower(a.name) ORDER BY a.name LIMIT 11 OFFSET ?`, offset)
	if err != nil {
		return err
	}
	type entry struct{ name, chat string }
	var users []entry
	for rows.Next() {
		var u entry
		if err := rows.Scan(&u.name, &u.chat); err != nil {
			rows.Close()
			return err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	hasMore := len(users) > 10
	if hasMore {
		users = users[:10]
	}
	text := "Elige a la persona que quieres vincular. Luego escribe aquí su ID numérico de Telegram. La persona puede consultarlo enviando /id al bot.\n\nSi el nombre no aparece, primero asígnalo a una novedad en la aplicación web. 🔗 indica que ya tiene un ID vinculado."
	keyboard := make([][]telegramButton, 0, len(users)+2)
	for i, u := range users {
		status := "➕ Seleccionar"
		if u.chat != "" {
			status = "🔗 Cambiar ID"
		}
		text += fmt.Sprintf("\n%s — %s", status, u.name)
		keyboard = append(keyboard, []telegramButton{{Text: telegramShort(status+": "+u.name, 58), CallbackData: fmt.Sprintf("ul:%d:%d", offset, i)}})
	}
	if len(users) == 0 {
		text += "\n\nNo hay responsables guardados para vincular."
	}
	if hasMore || offset > 0 {
		var nav []telegramButton
		if offset > 0 {
			prev := offset - 10
			if prev < 0 {
				prev = 0
			}
			nav = append(nav, telegramButton{Text: "← Anterior", CallbackData: fmt.Sprintf("ua:%d", prev)})
		}
		if hasMore {
			nav = append(nav, telegramButton{Text: "Siguiente →", CallbackData: fmt.Sprintf("ua:%d", offset+10)})
		}
		keyboard = append(keyboard, nav)
	}
	keyboard = append(keyboard, []telegramButton{{Text: "← Usuarios vinculados", CallbackData: "u:0"}})
	return api.editMessage(ctx, chatID, existing.MessageID, text, telegramMarkup{InlineKeyboard: keyboard})
}

func (a *App) createTelegramLinkInvite(ctx context.Context, api *telegramAPI, adminID int64, message *telegramMessage, offset, index int) error {
	rows, err := a.db.Query(`SELECT a.name FROM assignees a GROUP BY lower(a.name) ORDER BY a.name LIMIT 11 OFFSET ?`, offset)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if index >= len(names) || index >= 10 {
		return api.editMessage(ctx, adminID, message.MessageID, "Ese responsable ya no está disponible. Vuelve a elegirlo.", telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "Elegir responsable", CallbackData: fmt.Sprintf("ua:%d", offset)}}}})
	}
	name := names[index]
	if _, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS telegram_manual_link_pending (chat_id TEXT PRIMARY KEY, name TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		return err
	}
	_, err = a.db.Exec(`INSERT INTO telegram_manual_link_pending(chat_id,name,updated_at) VALUES(?,?,?) ON CONFLICT(chat_id) DO UPDATE SET name=excluded.name,updated_at=excluded.updated_at`, strconv.FormatInt(adminID, 10), name, now())
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Seleccionaste a %s.\n\nPídele que envíe /id al bot y te comparta el número. Luego escribe aquí ese ID numérico para completar el vínculo.\n\nSi necesitas cancelar, pulsa 🏠 Limpiar y volver al menú.", name)
	return api.editMessage(ctx, adminID, message.MessageID, text, telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "← Elegir otra persona", CallbackData: fmt.Sprintf("ua:%d", offset)}}}})
}

func (a *App) redeemTelegramLinkInvite(ctx context.Context, api *telegramAPI, chatID int64, token string) error {
	if len(token) != 32 {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "El enlace de vinculación no es válido o venció."}, nil)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRow(`SELECT name FROM telegram_link_invites WHERE token=? AND expires_at>?`, token, time.Now().Unix()).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "El enlace de vinculación ya se usó o venció. Solicita uno nuevo al administrador."}, nil)
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM telegram_link_invites WHERE token=?`, token); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO telegram_contacts(name,chat_id,updated_at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET chat_id=excluded.chat_id,updated_at=excluded.updated_at`, name, strconv.FormatInt(chatID, 10), now()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": fmt.Sprintf("Chat vinculado correctamente a %s. Ya puedes consultar todas las novedades abiertas con los botones.", name), "reply_markup": telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "Ver novedades pendientes", CallbackData: "y:0"}}}}}, nil)
}

func (a *App) telegramCanAccess(chatID int64, observationID int, admins map[int64]struct{}) (bool, error) {
	if isTelegramAdmin(chatID, admins) {
		return true, nil
	}
	var count int
	err := a.db.QueryRow(`SELECT count(*) FROM observations o JOIN processes p ON p.id=o.process_id
		JOIN assignees a ON a.observation_id=o.id JOIN telegram_contacts t ON t.name=a.name
		WHERE o.id=? AND p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica') AND t.chat_id=?`, observationID, strconv.FormatInt(chatID, 10)).Scan(&count)
	return count > 0, err
}

func (a *App) telegramCanViewObservation(chatID int64, observationID int, admins map[int64]struct{}) (bool, error) {
	if isTelegramAdmin(chatID, admins) {
		return true, nil
	}
	var count int
	err := a.db.QueryRow(`SELECT count(*) FROM observations o JOIN processes p ON p.id=o.process_id WHERE o.id=? AND p.status<>'Archivado' AND o.status NOT IN ('Subsanada','No aplica') AND EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?)`, observationID, strconv.FormatInt(chatID, 10)).Scan(&count)
	return count > 0, err
}

func (a *App) telegramCanAccessProcess(chatID int64, processID int, year string, admins map[int64]struct{}) (bool, error) {
	if isTelegramAdmin(chatID, admins) {
		return true, nil
	}
	var count int
	err := a.db.QueryRow(`SELECT count(*) FROM processes p WHERE p.id=? AND p.status<>'Archivado' AND EXISTS(
		SELECT 1 FROM observations o WHERE o.process_id=p.id AND o.status NOT IN ('Subsanada','No aplica')
		AND (?='all' OR substr(p.accounting_date,1,4)=? OR substr(o.received_date,1,4)=? OR substr(o.created_at,1,4)=?))
		AND EXISTS(SELECT 1 FROM telegram_contacts t WHERE t.chat_id=?)`, processID, year, year, year, year, strconv.FormatInt(chatID, 10)).Scan(&count)
	return count > 0, err
}

func (a *App) sendTelegramProcessDocument(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, documentID int, year string, processID int) error {
	allowed, err := a.telegramCanAccessProcess(chatID, processID, year, admins)
	if err != nil {
		return err
	}
	if !allowed {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "No tienes acceso a la documentación de este proceso."}, nil)
	}
	var stored, filename, category, label string
	var version int
	err = a.db.QueryRow(`SELECT stored_name,filename,category,label,version FROM documents WHERE id=? AND process_id=?`, documentID, processID).Scan(&stored, &filename, &category, &label, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "El archivo ya no está disponible."}, nil)
	}
	if err != nil {
		return err
	}
	if !safeTelegramDocumentCategory(category) {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "El archivo tiene una categoría no válida."}, nil)
	}
	path := filepath.Join(a.documentDir, strconv.Itoa(processID), category, filepath.Base(stored))
	caption := strings.TrimSpace(label)
	if caption == "" {
		caption = filename
	}
	caption = fmt.Sprintf("%s · versión %d", caption, version)
	if err := api.sendDocumentFile(ctx, chatID, path, filename, caption); err != nil {
		return api.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "No se pudo enviar el archivo. Puede faltar en el servidor o superar el límite de Telegram."}, nil)
	}
	return nil
}

func safeTelegramDocumentCategory(category string) bool {
	if category == "orden" || category == "contrato" || category == "terminos" {
		return true
	}
	if len(category) != 18 || !strings.HasPrefix(category, "anexo-") {
		return false
	}
	for _, c := range category[6:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (a *App) telegramObservation(id int) (telegramObservation, error) {
	var item telegramObservation
	err := a.db.QueryRow(`SELECT o.id,p.id,p.code,p.object,o.document,o.summary,o.original,o.status,o.due_date,
		COALESCE((SELECT group_concat(a.name,', ') FROM assignees a WHERE a.observation_id=o.id),'')
		FROM observations o JOIN processes p ON p.id=o.process_id WHERE o.id=? AND p.status<>'Archivado'`, id).
		Scan(&item.ID, &item.ProcessID, &item.Code, &item.Object, &item.Document, &item.Summary, &item.Original, &item.Status, &item.DueDate, &item.Assignees)
	if err != nil {
		return item, err
	}
	rows, err := a.db.Query(`SELECT message || ' · ' || created_at FROM activity WHERE observation_id=? AND message LIKE 'Acción a tomar:%' ORDER BY id DESC`, id)
	if err != nil {
		return item, err
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			return item, err
		}
		item.Actions = append(item.Actions, action)
	}
	if err := rows.Err(); err != nil {
		return item, err
	}
	return item, err
}

func telegramNoveltyCallback(action string, id, offset int, year string, processID int) string {
	if processID < 1 {
		return fmt.Sprintf("%s:%d:%d", action, id, offset)
	}
	return fmt.Sprintf("%s:%d:%d:%s:%d", action, id, offset, year, processID)
}

func (a *App) editTelegramObservation(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, message *telegramMessage, id, offset int, confirm bool, year string, processID int) error {
	item, err := a.telegramObservation(id)
	if errors.Is(err, sql.ErrNoRows) {
		return api.editMessage(ctx, message.Chat.ID, message.MessageID, "Esta novedad ya no está disponible.", telegramMarkup{})
	}
	if err != nil {
		return err
	}
	canManage, err := a.telegramCanAccess(message.Chat.ID, id, admins)
	if err != nil {
		return err
	}
	text := telegramObservationText(item)
	var keyboard telegramMarkup
	if confirm {
		text += "\n\n⚠️ Antes de marcarla como solventada, confirma que Contabilidad ya revisó esta novedad. Si no la revisó, cancela y no la marques."
		keyboard.InlineKeyboard = [][]telegramButton{{{Text: "✅ Contabilidad revisó; marcar solventada", CallbackData: telegramNoveltyCallback("c", id, offset, year, processID)}}, {{Text: "Cancelar", CallbackData: telegramNoveltyCallback("x", id, offset, year, processID)}}}
	} else if canManage && item.Status != "Subsanada" && item.Status != "No aplica" {
		keyboard.InlineKeyboard = [][]telegramButton{{{Text: "Marcar como solventada", CallbackData: telegramNoveltyCallback("r", id, offset, year, processID)}}, {{Text: "← Volver a novedades", CallbackData: telegramNoveltyCallback("b", id, offset, year, processID)}}}
	} else {
		if !canManage && item.Status != "Subsanada" && item.Status != "No aplica" {
			text += "\n\nSolo el responsable asignado o administración puede marcar esta novedad como solventada."
		}
		keyboard.InlineKeyboard = [][]telegramButton{{{Text: "← Volver a novedades", CallbackData: telegramNoveltyCallback("b", id, offset, year, processID)}}}
	}
	return api.editMessage(ctx, message.Chat.ID, message.MessageID, text, keyboard)
}

func telegramObservationText(item telegramObservation) string {
	code := item.Code
	if code == "" {
		code = fmt.Sprintf("Proceso #%d", item.ProcessID)
	}
	document := strings.TrimSpace(item.Document)
	if document == "" {
		document = strings.TrimSpace(item.Summary)
	}
	if document == "" {
		document = "Novedad sin título"
	}
	text := fmt.Sprintf("📌 %s\n%s\n\nNovedad: %s\nEstado: %s\nFecha límite: %s\nResponsables: %s", code, item.Object, document, item.Status, telegramOrDash(item.DueDate), telegramOrDash(item.Assignees))
	if strings.TrimSpace(item.Summary) != "" && strings.TrimSpace(item.Summary) != document {
		text += "\n\nResumen:\n" + item.Summary
	}
	if strings.TrimSpace(item.Original) != "" {
		text += "\n\nDetalle recibido:\n" + item.Original
	}
	text += "\n\nAcciones para subsanar:"
	if len(item.Actions) == 0 {
		text += "\nTodavía no se han registrado acciones para esta novedad."
	} else {
		for _, action := range item.Actions {
			text += "\n• " + action
		}
	}
	return telegramShort(text, 3800)
}

func isTwoDigitCode(value string) bool {
	if len(value) != 2 {
		return false
	}
	return value[0] >= '0' && value[0] <= '9' && value[1] >= '0' && value[1] <= '9'
}

func (a *App) telegramResolutionChallenge(chatID int64) (telegramResolutionChallenge, error) {
	var challenge telegramResolutionChallenge
	err := a.db.QueryRow(`SELECT observation_id,process_id,year,message_id,code,created_at,attempts,input FROM telegram_resolution_challenges WHERE chat_id=?`, strconv.FormatInt(chatID, 10)).Scan(&challenge.ObservationID, &challenge.ProcessID, &challenge.Year, &challenge.MessageID, &challenge.Code, &challenge.CreatedAt, &challenge.Attempts, &challenge.Input)
	return challenge, err
}

func (a *App) clearTelegramResolutionChallenge(chatID int64) error {
	_, err := a.db.Exec(`DELETE FROM telegram_resolution_challenges WHERE chat_id=?`, strconv.FormatInt(chatID, 10))
	return err
}

func (a *App) incrementTelegramChallengeAttempts(chatID int64) (int, error) {
	_, err := a.db.Exec(`UPDATE telegram_resolution_challenges SET attempts=attempts+1 WHERE chat_id=?`, strconv.FormatInt(chatID, 10))
	if err != nil {
		return 0, err
	}
	var attempts int
	err = a.db.QueryRow(`SELECT attempts FROM telegram_resolution_challenges WHERE chat_id=?`, strconv.FormatInt(chatID, 10)).Scan(&attempts)
	return attempts, err
}

func telegramDigitKeyboard() telegramMarkup {
	rows := [][]telegramButton{}
	for start := 0; start < 10; start += 5 {
		row := make([]telegramButton, 0, 5)
		for digit := start; digit < start+5; digit++ {
			row = append(row, telegramButton{Text: strconv.Itoa(digit), CallbackData: "dg:" + strconv.Itoa(digit)})
		}
		rows = append(rows, row)
	}
	return telegramMarkup{InlineKeyboard: rows}
}

func (a *App) handleTelegramResolutionDigit(ctx context.Context, api *telegramAPI, admins map[int64]struct{}, chatID int64, message *telegramMessage, digit string) error {
	challenge, err := a.telegramResolutionChallenge(chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return api.editMessage(ctx, chatID, message.MessageID, "La confirmación ya no está activa. La novedad sigue pendiente.", telegramMarkup{})
	}
	if err != nil {
		return err
	}
	if time.Now().Unix()-challenge.CreatedAt > 300 {
		_ = a.clearTelegramResolutionChallenge(chatID)
		return api.editMessage(ctx, chatID, challenge.MessageID, "El código venció. La novedad sigue pendiente; vuelve a iniciar la confirmación.", telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "Ver detalle", CallbackData: telegramNoveltyCallback("v", challenge.ObservationID, 0, challenge.Year, challenge.ProcessID)}}}})
	}
	index := len(challenge.Input)
	if index > 1 {
		challenge.Input = ""
		index = 0
	}
	if challenge.Code[index:index+1] != digit {
		attempts, err := a.incrementTelegramChallengeAttempts(chatID)
		if err != nil {
			return err
		}
		if attempts >= 3 {
			_ = a.clearTelegramResolutionChallenge(chatID)
			return api.editMessage(ctx, chatID, challenge.MessageID, "Se agotaron los intentos. La novedad sigue pendiente; vuelve a iniciar la confirmación.", telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "Ver detalle", CallbackData: telegramNoveltyCallback("v", challenge.ObservationID, 0, challenge.Year, challenge.ProcessID)}}}})
		}
		if _, err = a.db.Exec(`UPDATE telegram_resolution_challenges SET input='' WHERE chat_id=?`, strconv.FormatInt(chatID, 10)); err != nil {
			return err
		}
		return api.editMessage(ctx, chatID, challenge.MessageID, fmt.Sprintf("Esa cifra no coincide. Código: %s. Inténtalo de nuevo; intento %d de 3. Toca primero una cifra y luego la otra.", challenge.Code, attempts+1), telegramDigitKeyboard())
	}
	if index == 0 {
		if _, err = a.db.Exec(`UPDATE telegram_resolution_challenges SET input=? WHERE chat_id=?`, digit, strconv.FormatInt(chatID, 10)); err != nil {
			return err
		}
		return api.editMessage(ctx, chatID, challenge.MessageID, fmt.Sprintf("Primera cifra correcta. Toca la segunda. Código: %s", challenge.Code), telegramDigitKeyboard())
	}
	if err := a.resolveTelegramObservation(ctx, admins, chatID, challenge.ObservationID); err != nil {
		_ = a.clearTelegramResolutionChallenge(chatID)
		return api.editMessage(ctx, chatID, challenge.MessageID, "No se pudo marcar. La novedad puede haber cambiado o ya no tienes acceso.", telegramMarkup{})
	}
	if err := a.clearTelegramResolutionChallenge(chatID); err != nil {
		return err
	}
	return api.editMessage(ctx, chatID, challenge.MessageID, "✅ Código correcto. Novedad marcada como solventada; quedó registrada tu confirmación de revisión de Contabilidad.", telegramMarkup{InlineKeyboard: [][]telegramButton{{{Text: "Volver a todas las novedades", CallbackData: fmt.Sprintf("n:%s:%d:0", challenge.Year, challenge.ProcessID)}}, {{Text: "Elegir otro año", CallbackData: "y:0"}}}})
}

func (a *App) startTelegramResolutionChallenge(ctx context.Context, api *telegramAPI, chatID int64, message *telegramMessage, id int, year string, processID int) error {
	codeNumber, err := cryptorand.Int(cryptorand.Reader, big.NewInt(100))
	if err != nil {
		return err
	}
	code := fmt.Sprintf("%02d", codeNumber.Int64())
	_, err = a.db.Exec(`INSERT INTO telegram_resolution_challenges(chat_id,observation_id,process_id,year,message_id,code,created_at,attempts,input) VALUES(?,?,?,?,?,?,?,0,'')
		ON CONFLICT(chat_id) DO UPDATE SET observation_id=excluded.observation_id,process_id=excluded.process_id,year=excluded.year,message_id=excluded.message_id,code=excluded.code,created_at=excluded.created_at,attempts=0,input=''`, strconv.FormatInt(chatID, 10), id, processID, year, message.MessageID, code, time.Now().Unix())
	if err != nil {
		return err
	}
	text := fmt.Sprintf("⚠️ Confirma solo si Contabilidad ya revisó esta novedad. Si no la revisó, cancela y no la marques.\n\nCódigo: %s\n\nToca sus dos cifras en los botones, primero la primera y luego la segunda. No escribas nada. Tienes 5 minutos y 3 intentos.", code)
	markup := telegramDigitKeyboard()
	markup.InlineKeyboard = append(markup.InlineKeyboard, []telegramButton{{Text: "Cancelar; dejar pendiente", CallbackData: telegramNoveltyCallback("x", id, 0, year, processID)}})
	return api.editMessage(ctx, chatID, message.MessageID, text, markup)
}

func (a *App) resolveTelegramObservation(ctx context.Context, admins map[int64]struct{}, chatID int64, id int) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	allowed := isTelegramAdmin(chatID, admins)
	if !allowed {
		var count int
		err = tx.QueryRow(`SELECT count(*) FROM observations o JOIN processes p ON p.id=o.process_id JOIN assignees a ON a.observation_id=o.id JOIN telegram_contacts t ON t.name=a.name WHERE o.id=? AND p.status<>'Archivado' AND t.chat_id=?`, id, strconv.FormatInt(chatID, 10)).Scan(&count)
		if err != nil {
			return err
		}
		allowed = count > 0
	}
	if !allowed {
		return errors.New("sin acceso a la novedad")
	}
	res, err := tx.Exec(`UPDATE observations SET status='Subsanada' WHERE id=? AND status NOT IN ('Subsanada','No aplica') AND process_id IN (SELECT id FROM processes WHERE status<>'Archivado')`, id)
	if err != nil {
		return err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return errors.New("la novedad ya no está pendiente")
	}
	messageText := fmt.Sprintf("Telegram: el chat %d ingresó el código de confirmación y confirmó que Contabilidad revisó la novedad; marcada como solventada", chatID)
	if _, err = tx.Exec("INSERT INTO activity(observation_id,message,created_at) VALUES(?,?,?)", id, messageText, now()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (api *telegramAPI) answerCallback(ctx context.Context, id, text string, alert bool) error {
	return api.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text, "show_alert": alert}, nil)
}

func (api *telegramAPI) editMessage(ctx context.Context, chatID int64, messageID int, text string, markup telegramMarkup) error {
	markup = telegramMarkupWithMenu(markup)
	return api.call(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": markup}, nil)
}

func telegramMarkupWithMenu(markup telegramMarkup) telegramMarkup {
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.CallbackData == "m:0" {
				return markup
			}
		}
	}
	markup.InlineKeyboard = append(markup.InlineKeyboard, []telegramButton{{Text: "🏠 Limpiar y volver al menú", CallbackData: "m:0"}})
	return markup
}

func (a *App) telegramUpdateOffset() (int64, error) {
	var value string
	err := a.db.QueryRow("SELECT value FROM telegram_state WHERE key='update_offset'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

func (a *App) saveTelegramUpdateOffset(offset int64) error {
	_, err := a.db.Exec(`INSERT INTO telegram_state(key,value) VALUES('update_offset',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(offset, 10))
	return err
}

func isTelegramAdmin(chatID int64, admins map[int64]struct{}) bool {
	_, ok := admins[chatID]
	return ok
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func telegramOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
func telegramShort(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func waitContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
