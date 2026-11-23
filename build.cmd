@echo off
setlocal
set "ROOT=%~dp0"
pushd "%ROOT%"

if not exist ".env" (
  echo Missing .env. Copy .env.example to .env and configure it first.
  popd
  exit /b 1
)

if not exist "dist" mkdir "dist"
if errorlevel 1 (
  echo Could not create dist folder.
  popd
  exit /b 1
)

echo Building gpn-webservice.exe...
go build -o "dist\gpn-webservice.exe" .
if errorlevel 1 (
  echo Build failed.
  popd
  exit /b 1
)

copy /Y ".env" "dist\.env" >nul
if errorlevel 1 (
  echo Could not copy .env to dist.
  popd
  exit /b 1
)

copy /Y "dist\gpn-webservice.exe" "gpn-webservice.exe" >nul
if errorlevel 1 (
  echo Package in dist is updated. Close the running server to update the root executable.
) else (
  echo Build complete. Both root and dist executables are updated.
)
echo Copy the entire dist folder to the target computer.
echo Start it there with: gpn-webservice.exe
popd
endlocal
