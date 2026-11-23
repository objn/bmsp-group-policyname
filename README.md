# Go Web Service + PostgreSQL

เว็บเซอร์วิสตัวอย่างด้วย Go เสิร์ฟหน้าเว็บและเชื่อม PostgreSQL โดยสร้างตารางอัตโนมัติเมื่อเริ่มทำงาน

## เริ่มใช้งานด้วย Docker Compose

ต้องติดตั้ง Docker Desktop แล้วรัน:

```sh
docker compose up --build
```

เปิดเว็บที่ http://localhost:8080

- `GET /healthz` สถานะเว็บ
- `GET /readyz` สถานะการเชื่อมต่อ PostgreSQL

## รัน Go บนเครื่อง

1. เริ่ม PostgreSQL และสร้างฐานข้อมูล (ค่าเริ่มต้น `gpn`/`gpn`/`gpn`)
2. ตั้งค่า `DATABASE_URL` หรือ `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_SSLMODE`
3. ติดตั้ง Air สำหรับ hot reload: `go install github.com/air-verse/air@latest`
4. รัน `air`; เมื่อแก้ไฟล์ Go, HTML, CSS หรือ JS server จะ build และ restart ให้อัตโนมัติ

หากไม่ต้องการ hot reload ใช้ `go run .` กำหนดพอร์ตเว็บด้วย `ADDR` (ค่าเริ่มต้น `:8080`)

## Build สำหรับย้ายไปรันบน Windows

รัน `build.cmd` จาก CMD หรือดับเบิลคลิก จะ build `gpn-webservice.exe` ในโฟลเดอร์โปรเจกต์และสร้างโฟลเดอร์ `dist` ที่มี `gpn-webservice.exe` และ `.env` อยู่คู่กัน ให้คัดลอกทั้งโฟลเดอร์ `dist` ไปยังเครื่องปลายทาง แล้วเปิด CMD ในโฟลเดอร์นั้นและรัน `gpn-webservice.exe` เครื่องปลายทางต้องติดตั้งและเปิด PostgreSQL ตามค่าใน `.env` ก่อน

ไฟล์ `.env` มีข้อมูลการเชื่อมต่อฐานข้อมูลและค่า login จึงไม่ควรส่งผ่านช่องทางสาธารณะหรือ commit เข้า Git

## Database init

ฟังก์ชัน `initializeDatabase` ใน `main.go` ใช้ `CREATE TABLE IF NOT EXISTS` ทุกครั้งเมื่อเริ่ม service จึงสร้าง `app_settings`, `visits` และ index ที่ขาด โดยไม่ลบข้อมูลเดิม หากเพิ่มตารางที่ต้องใช้ ให้เพิ่มคำสั่ง SQL ในฟังก์ชันนี้

## ปรับค่ารหัสผ่าน

ค่าใน compose เป็นค่าตัวอย่างสำหรับการพัฒนา ควรเปลี่ยนก่อนใช้งานจริง


## XLSX Import

จากหน้า Policy Center กด **นำเข้า XLSX** แล้วเลือก workbook ที่มี worksheet `In Scope` และคอลัมน์ `Policy Name`, `Criticality`, `Category`, `Enabled`, `Tenant`, `Device Category`

- `Enabled` หรือ `Disabled` จะถูกบันทึกเป็น `active` หรือ `in-active`
- Tenant values are stored together in one field, comma-separated, for example `site1,site2`.
- เพิ่มแถวที่ยังไม่มีในฐานข้อมูล; แถวที่ไม่อยู่ในไฟล์จะคงเดิม
- การ import ทำใน transaction เดียว หากผิดพลาดจะ rollback ทั้งหมด

## Import behavior

XLSX imports are append-only comparisons. Record IDs are ignored. Exact matches across all policy fields are reported as unchanged. New records are inserted; when a matching policy identity has different field values, the imported row is inserted as a new version and the differing fields are reported. Existing database rows that are absent from the workbook remain untouched. Remove rows manually through the UI when needed.

Policy Name is the matching key. When policy fields match, tenant values are merged into the existing comma-separated tenant list. If other policy fields differ, the import adds a new version row. Existing rows are never removed by import.

XLSX import now reads the No. column as the primary key (id): matching No. rows are updated, new No. rows are inserted, Tenant values are merged as a comma-separated set, and database rows absent from the workbook are retained. For the first import after the old generated-ID scheme, matching legacy rows are re-keyed to the workbook No. using their policy name.

Current sync behavior: database rows are made to match workbook No. values exactly. Matching No. rows are updated, missing No. values are inserted, and database rows whose No. is absent from the workbook are removed. The post-import log includes only changed rows (added, updated, removed), and can be downloaded as CSV from the report dialog.

## Login

The app requires `ADMIN_USER` and `ADMIN_PASSWORD_SHA256` in `.env`. The SHA-256 digest is calculated from the UTF-8 password; the app compares the digest and does not store the plaintext password. Sessions use an HttpOnly, SameSite=Strict cookie with a 12-hour expiry. Set `COOKIE_SECURE=true` when serving over HTTPS.
