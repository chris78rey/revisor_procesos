# PRD: Revisor y seguimiento de procesos de contratación

## 1. Resumen

Aplicación local para registrar y dar seguimiento a procesos de contratación y a las novedades u objeciones que reciben sus expedientes, especialmente durante el trámite de pago. Debe facilitar el registro desde texto copiado de portales, correos o reportes de distintas áreas; organizar las correcciones y responsables; alertar sobre tareas y plazos; y producir reportes consolidados.

La solución se implementará en Go con SQLite. OpenRouter se utilizará como apoyo para interpretar texto, proponer datos y responder consultas sobre la información registrada. La persona usuaria revisará y confirmará los datos sugeridos antes de guardarlos.

## 2. Problema

La información de un proceso y sus observaciones llega en formatos distintos y puede involucrar varios documentos, áreas y responsables. El seguimiento manual dificulta saber qué corregir, quién lo hará, qué devoluciones siguen pendientes y cuánto tiempo queda para ejecutar una obligación. Las nuevas devoluciones deben conservar el historial y no borrar las anteriores.

## 3. Objetivos

- Registrar procesos de forma rápida pegando texto y completando o confirmando los campos detectados.
- Reunir datos del proceso, novedades, responsables, fechas y actuaciones en un solo lugar.
- Controlar la subsanación desde la recepción de una observación hasta su resolución.
- Mostrar pendientes y alertas de plazos en un panel sencillo.
- Generar reportes consolidados y permitir consultas en lenguaje natural sobre los datos registrados.
- Mantener los datos en SQLite y evitar almacenar los documentos del expediente en la aplicación.

## 4. Alcance

### Incluido

- Procesos de ínfima cuantía y otros tipos de contratación, incluida subasta inversa electrónica.
- Registro manual y asistido a partir de texto pegado.
- URL del proceso y datos relevantes copiados de su página.
- Registro de observaciones de una o varias áreas y de sucesivas devoluciones.
- Asignación de una o varias personas responsables por observación.
- Estados, prioridad, fechas límite, comentarios y bitácora de actuaciones.
- Seguimiento de plazos de ejecución y fechas configuradas para alertas.
- Panel de pendientes, alertas y reportes filtrables.
- Búsqueda de procesos y consultas conversacionales acotadas a la información guardada.

### Fuera de alcance inicial

- Guardar, versionar, editar o distribuir los documentos del expediente. La aplicación registra referencias y observaciones, pero no es un repositorio documental.
- Conectarse automáticamente a portales de contratación o extraer datos directamente de ellos.
- Enviar notificaciones externas (correo, SMS o mensajería) mientras no se defina un canal.
- Reemplazar los sistemas oficiales ni determinar por sí sola el cumplimiento legal de un proceso.

## 5. Usuarios y permisos

El producto está dirigido inicialmente a una persona o equipo que registra procesos, coordina correcciones y consulta el estado de los expedientes. El modelo de acceso (usuario único o varios usuarios, autenticación y permisos) está pendiente de definición. La asignación de responsables no implica que cada responsable tenga una cuenta en el sistema.

## 6. Datos del proceso

Cada proceso podrá registrar, según disponibilidad:

- Código o número de proceso y tipo de contratación.
- Entidad, objeto o descripción de la contratación y estado.
- URL del proceso.
- Proveedor o proveedores, identificación y razón social.
- Presupuesto o valor de referencia, valor de factura y forma de pago.
- Fecha de publicación, fecha límite de proformas y fechas relevantes del trámite.
- Plazo de ejecución, unidad del plazo y fecha desde la que se cuenta.
- Funcionario encargado y datos de contacto que sean pertinentes.
- Fecha de creación y última actualización del registro.

Los campos no aplicables o todavía desconocidos podrán quedar vacíos. El sistema no debe inventar datos faltantes.

## 7. Observaciones y subsanación

Una observación representa una novedad concreta comunicada por un área sobre el expediente o el proceso. Debe incluir, cuando se conozcan:

- Área que la emite y fecha de recepción.
- Documento o sección observada, como referencia textual (por ejemplo, acta de entrega recepción, informe de conformidad u oficio).
- Texto original de la observación y descripción resumida editable.
- Prioridad y estado.
- Una o varias personas responsables.
- Fecha límite de corrección, si fue definida.
- Registro cronológico de comentarios, cambios de estado y otras actuaciones.
- Relación opcional con una observación anterior para identificar recurrencias, sin reemplazar ni borrar el registro previo.

Estados iniciales propuestos: **Pendiente**, **En corrección**, **En revisión**, **Subsanada** y **No aplica**. Los estados y la posibilidad de reabrir una observación deberán ser configurables o confirmados antes de implementación.

Al recibir nuevas novedades, el sistema crea registros nuevos. No sobrescribe las observaciones anteriores. La persona usuaria podrá indicar si una observación nueva repite o amplía otra existente.

## 8. Registro asistido con OpenRouter

El flujo esperado es:

1. La persona usuaria pega texto libre o contenido tabular de un portal, correo o reporte.
2. El sistema solicita a OpenRouter una propuesta estructurada de datos del proceso y, si corresponde, una lista de observaciones.
3. La interfaz presenta los campos detectados, el texto fuente y los datos que no se pudieron determinar.
4. La persona usuaria corrige, completa y confirma la propuesta.
5. Solo después de la confirmación se guardan los datos en SQLite.

La extracción debe tolerar formatos variables, separar observaciones cuando sea posible y conservar el texto original para revisión. La IA puede sugerir clasificación, prioridad, responsables o fechas solo cuando haya evidencia en el texto; toda sugerencia debe mostrarse como tal y ser editable. Ante ambigüedad, debe pedir confirmación en pantalla en vez de completar por conjetura.

El buscador conversacional podrá responder preguntas como «¿Qué observaciones tiene pendientes Bluefrog?», «¿Quién está a cargo de corregir el acta?» o «¿Cuántos días quedan para cumplir el contrato?». Las respuestas deben basarse en registros locales, señalar cuando falten datos y permitir abrir los procesos u observaciones relacionados. La IA no debe ejecutar cambios en los registros sin confirmación explícita.

### Manejo de credenciales y datos enviados

- La clave de OpenRouter se configurará fuera de la base de datos y del código fuente, mediante un mecanismo local seguro por definir.
- No se incluirán claves en logs, reportes ni mensajes de error.
- Antes de enviar texto a OpenRouter, la interfaz indicará que se procesará mediante un servicio externo. El alcance de los datos que pueden enviarse y las opciones de anonimización quedan pendientes de definición institucional.
- Si OpenRouter no está disponible o no hay credencial configurada, el registro manual y la consulta local básica deben seguir funcionando.

## 9. Panel, alertas y reportes

El panel debe priorizar:

- Observaciones pendientes, vencidas y próximas a vencer.
- Procesos con plazos de ejecución próximos o vencidos.
- Tareas agrupadas por responsable, área, proceso y prioridad.
- Últimas actuaciones y devoluciones recibidas.

Las alertas se calcularán a partir de fechas registradas y reglas configurables. No se asumirá una regla legal de cómputo de días: debe definirse si los plazos usan días calendario o laborables, feriados, hora límite y zona horaria. Mientras esto no esté definido, el sistema debe mostrar la fecha fuente y presentar cualquier cálculo como referencial.

Los reportes consolidados deben permitir filtrar al menos por periodo, estado, área, responsable, proveedor y tipo de contratación. Deben resumir procesos y observaciones abiertas/cerradas, vencimientos y actuaciones. Formatos de exportación y plantillas quedan por definir.

## 10. Requisitos funcionales

- **RF-01:** Crear, consultar, editar y archivar procesos sin eliminar el historial de observaciones.
- **RF-02:** Registrar proceso mediante formulario manual o pegado de texto.
- **RF-03:** Mostrar y permitir editar las sugerencias de extracción antes de guardarlas.
- **RF-04:** Registrar varias observaciones por proceso y varias personas responsables por observación.
- **RF-05:** Mantener historial cronológico de actuaciones y cambios de estado.
- **RF-06:** Buscar por código, objeto, proveedor, URL, observación, área o responsable.
- **RF-07:** Mostrar panel de pendientes, prioridades y alertas por fecha.
- **RF-08:** Crear reportes consolidados con filtros.
- **RF-09:** Responder consultas conversacionales utilizando los registros disponibles y enlazar los resultados con sus registros fuente.
- **RF-10:** Permitir el trabajo manual cuando el servicio de IA no esté disponible.

## 11. Requisitos no funcionales

- Backend en Go y persistencia local en SQLite.
- Datos consistentes ante cierres inesperados; definir estrategia de copias de seguridad y restauración antes de producción.
- Interfaz simple, orientada a registro rápido y revisión clara de pendientes.
- La IA debe devolver datos estructurados validados por la aplicación; la salida del modelo no se ejecutará como SQL ni como código.
- Registrar fechas de forma consistente y mostrar claramente la zona horaria utilizada.
- Los errores de conexión con OpenRouter no deben impedir consultar o editar información local.

## 12. Criterios de aceptación del MVP

1. Se puede crear un proceso manualmente con código, tipo, objeto, proveedor, URL y fechas disponibles.
2. Al pegar un texto de proceso, la aplicación presenta campos sugeridos para revisión y no los persiste hasta confirmar.
3. Se pueden registrar varias observaciones provenientes de áreas distintas, asignar responsables y actualizar su estado.
4. Las devoluciones posteriores se agregan al historial y no eliminan las anteriores.
5. El panel identifica observaciones pendientes y fechas vencidas o próximas según una regla de anticipación configurable.
6. Se puede filtrar y consultar un reporte consolidado de procesos y observaciones.
7. Una consulta conversacional responde usando datos existentes y reconoce cuando la información requerida no está registrada.
8. Sin conexión o credencial de OpenRouter, siguen disponibles las funciones manuales y la consulta local básica.
9. La aplicación no almacena los archivos de documentos del expediente.

## 13. Decisiones pendientes

- Nombre definitivo del producto y plataforma de uso (escritorio local o aplicación web local/red interna).
- Uno o varios usuarios; autenticación, permisos y acceso concurrente.
- Sistema operativo y forma de instalación/actualización.
- Mecanismo exacto para proteger y configurar la clave de OpenRouter.
- Tipos de contenido que pueden enviarse al proveedor de IA y necesidad de anonimización.
- Reglas de cómputo de días, feriados, alertas previas y calendario institucional.
- Catálogo de áreas, prioridades, estados y responsables; si serán administrables.
- Formatos de exportación de reportes y copias de seguridad.
- Retención, archivado y eliminación de registros.

## 14. Ejemplos de entrada a considerar

El diseño debe admitir, sin depender de una plantilla única:

- Texto de detalle de una necesidad de ínfima cuantía, con entidad, código, objeto, fechas, funcionario, proveedor y URL.
- Reportes tabulares de novedades de expedientes de pago con fecha, proceso, factura, documento observado, detalle, valor, responsables y proveedor.
- Fichas de subasta inversa electrónica con código, objeto, presupuesto, forma de pago, plazo de entrega, funcionario y estado.

Estos ejemplos son fuentes de prueba para el diseño de extracción. Los nombres, códigos, valores y contactos presentes en ejemplos reales no deben convertirse en catálogos fijos ni en datos precargados del producto.
