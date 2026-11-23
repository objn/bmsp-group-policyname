# Go Web Service + PostgreSQL

เว็บแอปจัดการ Policy ด้วย Go และ PostgreSQL แอปเชื่อมต่อกับ PostgreSQL ที่มีอยู่แล้วเท่านั้น ไม่ได้ติดตั้งหรือเปิด PostgreSQL และไม่สร้างหรือแก้ schema อัตโนมัติ

## 1. ตั้งค่า `.env`

คัดลอกไฟล์ตัวอย่างแล้วแก้ค่าการเชื่อมต่อฐานข้อมูล:

```cmd
copy .env.example .env
notepad .env
```

ตั้ง `DATABASE_URL` ให้ชี้ไปยัง PostgreSQL ที่มีอยู่แล้ว และตั้ง `ADMIN_USER` กับ `ADMIN_PASSWORD_SHA256` สำหรับ login โดยค่า password ต้องเป็น SHA-256 ของรหัสผ่านแบบ UTF-8 แล้วเขียนเป็น hex 64 ตัว

สร้าง hash บน Windows PowerShell ด้วยคำสั่งนี้ แล้วนำค่าที่ได้ไปใส่ใน `.env`:

```powershell
$sha = [Security.Cryptography.SHA256]::Create()
[BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes('your-password'))).Replace('-', '').ToLowerInvariant()
```

## 2. Build และรันบน Windows

ติดตั้ง Go 1.24 ขึ้นไป แล้วเปิด CMD ในโฟลเดอร์โปรเจกต์ จากนั้น build และเริ่มเว็บ:

```cmd
go build -o gpn-webservice.exe .
gpn-webservice.exe
```

หรือรัน `build.cmd` เพื่อสร้างแพ็กเกจ `dist` ที่มี `gpn-webservice.exe` และ `.env` อยู่ด้วยกัน คัดลอกทั้งโฟลเดอร์ไปเครื่องปลายทาง แล้วรัน `gpn-webservice.exe` จากในโฟลเดอร์นั้น

## Build และรันด้วย Docker

Docker ใช้เฉพาะรันเว็บเซอร์วิส โดยต้องตั้ง `DATABASE_URL` ให้เข้าถึง PostgreSQL จาก container ได้:

```sh
docker build -t policy-center .
docker run --rm -p 8080:8080 --env-file .env policy-center
```

ถ้าฐานข้อมูลอยู่บน Windows host ให้ใช้ `host.docker.internal` แทน `localhost` ใน `DATABASE_URL` จากนั้นเปิด http://localhost:8080

## ตรวจสถานะ

- `GET /healthz` ตรวจว่าเว็บเซอร์วิสทำงาน
- `GET /readyz` ตรวจการเชื่อมต่อ PostgreSQL

ไฟล์ `.env` มีข้อมูลการเชื่อมต่อฐานข้อมูลและค่า login จึงไม่ควรส่งผ่านช่องทางสาธารณะหรือ commit เข้า Git

## Database schema

แอปเชื่อมต่อและตรวจสถานะ PostgreSQL เท่านั้น ไม่มีการสร้าง แก้ไข หรือลบตารางเมื่อเริ่มทำงาน หากต้องสร้างตาราง `policy_list_filtered` ให้ตรวจสอบและรัน `init.sql` ด้วยตนเองก่อน โดยผู้ดูแลฐานข้อมูลสามารถตรวจ schema และข้อมูลก่อนรันได้

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
