# Revisor de procesos

Aplicación web local para el seguimiento de procesos de contratación y subsanación de observaciones. Está escrita en Go y guarda sus datos en SQLite. Conserva los documentos del expediente en una carpeta local junto a la base de datos, con versiones numeradas por tipo.

## Requisitos

- Go 1.22 o superior.
- Acceso a internet solo para descargar dependencias durante la primera compilación y, opcionalmente, usar OpenRouter.

## Ejecutar

```sh
go run .
```

Abre <http://127.0.0.1:8080>. La base SQLite se crea por defecto en `data/revisor.db` y los adjuntos se guardan en `data/documents/`. Para cambiar ubicación o puerto:

```sh
REVISOR_DB=/ruta/segura/revisor.db REVISOR_ADDR=127.0.0.1:8080 go run .
```

La app escucha solo en `127.0.0.1` por defecto. Para acceso desde la red local se puede configurar `REVISOR_ADDR`, pero la versión actual no incluye autenticación; no la expongas a una red compartida sin proteger el acceso.

## Configurar OpenRouter (opcional)

En la aplicación abre **Ajustes** (arriba a la derecha) o visita <http://127.0.0.1:8080/settings>. Allí puedes indicar el identificador del modelo y la API key. El identificador inicial es `openai/gpt-4o-mini`; puedes cambiarlo por cualquier modelo disponible en tu cuenta de OpenRouter.

La clave no se muestra después de guardarla. Se conserva en `data/settings.json`, al lado de SQLite, con permisos de lectura/escritura solo para el usuario local. El archivo no se cifra; protege tu sesión de usuario y no compartas la carpeta. La clave no se guarda en SQLite.

También puedes configurar variables de entorno, que tienen prioridad sobre los Ajustes:

```sh
export OPENROUTER_API_KEY='tu-clave'
export OPENROUTER_MODEL='openai/gpt-4o-mini'
go run .
```

La extracción envía el texto pegado a OpenRouter solo cuando se pulsa **Ordenar texto**. La consulta conversacional también envía la pregunta y un conjunto acotado de registros locales cuando la clave está configurada. Sin clave, el registro manual y la búsqueda local siguen disponibles.

## Bot de Telegram (opcional)

El archivo local `.env` admite `TELEGRAM_BOT_TOKEN` y `TELEGRAM_ADMIN_CHAT_IDS`. Copia la plantilla `.env.example` si necesitas recrearlo. No compartas ni publiques `.env`.

Inicia el bot y usa sus botones; no hace falta escribir comandos para consultar procesos. Desde cualquier pantalla, **🏠 Limpiar y volver al menú** reemplaza el contenido actual por el menú principal. Cualquier chat vinculado ve todas las novedades abiertas de todos los procesos no archivados. Para agregar a una persona, el administrador pulsa **Agregar responsable** en el menú, elige su nombre y escribe el ID numérico de Telegram de esa persona. La persona puede obtenerlo enviando `/id` al bot y compartiendo el número con el administrador. El nombre debe existir como responsable en una novedad de la aplicación web. Solo el responsable asignado o un administrador puede marcar una novedad como solventada.

Al abrir un proceso, aparecen juntas sus novedades y los documentos disponibles. Pulsa **Descargar** para recibir un archivo del expediente en Telegram. Para solventar una novedad, confirma primero que Contabilidad la revisó y toca las dos cifras del teclado numérico del bot; no hay que escribir el código. El bot registra esa confirmación en el historial. Además, al inicio de cada hora local de Ecuador envía a cada chat vinculado un resumen de todos los procesos de este año con novedades abiertas y botones para abrirlos. No envía un aviso si no hay novedades abiertas.

## Copia de seguridad

Detén la aplicación y copia el archivo SQLite, su archivo `-wal` si existe y la carpeta `data/documents/`. Para una copia en caliente de SQLite, usa la utilidad `sqlite3` con el comando `.backup`; copia también la carpeta de documentos:

```sh
sqlite3 data/revisor.db ".backup 'revisor-backup.db'"
```

## Estado del MVP

Incluye creación, edición, archivo y reactivación de procesos; detección de códigos repetidos; búsqueda histórica por texto y año; reporte de novedades filtrable por año, estado y responsable con paginación y exportación CSV de todos los resultados filtrados; ordenamiento asistido de texto libre, tablas, correos y reportes con revisión antes de guardar; campos de resolución, justificación, modalidad, adjudicación, funcionario, fechas de control, rubro, precios y calificaciones; ítems múltiples por proceso con CPC, rubro, descripción, cantidad, unidad, precio unitario y subtotal; proveedor/RUC y forma de pago; novedades con responsables independientes e historial; documentos de orden, términos de referencia y anexos con nombre propio, versiones anteriores conservadas; consultas opcionales con OpenRouter. El cómputo de días usa fecha calendario de la máquina y es referencial; no aplica feriados ni determina cumplimiento normativo.
