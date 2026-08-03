@echo off
REM pm shim para Windows: reenvia cualquier comando al binario pm dentro de WSL.
REM Copia este archivo a una carpeta que este en el PATH de Windows
REM (p.ej. C:\Users\<tu-usuario>\bin) y podras usar `pm ps`, `pm start x`,
REM `pm ui`, etc. desde PowerShell o cmd igual que dentro de WSL.
wsl.exe -e pm %*
