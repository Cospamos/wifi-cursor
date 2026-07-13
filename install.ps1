<#
    Устанавливает wifi-cursor: качает готовый .exe из GitHub Releases,
    спрашивает куда установить и нужно ли добавить в PATH.

    Использование:
      .\install.ps1
      .\install.ps1 -InstallDir "C:\Tools\wifi-cursor" -AddToPath y
      .\install.ps1 -Version v0.1.0
#>
[CmdletBinding()]
param(
    [string]$InstallDir,
    [string]$AddToPath,
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"

$Repo      = "Cospamos/wifi-cursor"
$AssetName = "wifi-cursor-windows-amd64.exe"

function Get-DownloadUrl {
    param([string]$Ver)
    if ($Ver -eq "latest") {
        return "https://github.com/$Repo/releases/latest/download/$AssetName"
    }
    return "https://github.com/$Repo/releases/download/$Ver/$AssetName"
}

Write-Host "=== wifi-cursor installer ===" -ForegroundColor Cyan

if (-not $InstallDir) {
    $default = Join-Path $env:LOCALAPPDATA "wifi-cursor"
    $answer = Read-Host "Куда установить wifi-cursor? [$default]"
    $InstallDir = if ([string]::IsNullOrWhiteSpace($answer)) { $default } else { $answer }
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exePath = Join-Path $InstallDir "wifi-cursor.exe"
$url = Get-DownloadUrl -Ver $Version

Write-Host "Скачиваю $url"
Invoke-WebRequest -Uri $url -OutFile $exePath -UseBasicParsing

Write-Host "Установлено: $exePath" -ForegroundColor Green

if (-not $AddToPath) {
    $answer = Read-Host "Добавить wifi-cursor в PATH, чтобы запускать из любой папки? (y/n) [y]"
    $AddToPath = if ([string]::IsNullOrWhiteSpace($answer)) { "y" } else { $answer }
}

if ($AddToPath -match '^(?i:y|yes|д|да)') {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $parts = @()
    if ($userPath) { $parts = $userPath -split ";" | Where-Object { $_ -ne "" } }
    if ($parts -notcontains $InstallDir) {
        $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        $env:Path = "$env:Path;$InstallDir"
        Write-Host "Добавлено в PATH (пользовательский). Откройте новое окно терминала, чтобы изменения подхватились везде." -ForegroundColor Yellow
    } else {
        Write-Host "Уже есть в PATH." -ForegroundColor Yellow
    }
} else {
    Write-Host "PATH не трогаю. Запускать так: `"$exePath`"" -ForegroundColor Yellow
}

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
# Только private/domain (домашние и корпоративные сети) — на публичных/гостевых
# Wi-Fi (profile=public) правило нарочно не открывается.
$fwRuleCmd = "netsh advfirewall firewall add rule name=`"wifi-cursor`" dir=in action=allow program=`"$exePath`" enable=yes profile=private,domain"
if ($isAdmin) {
    netsh advfirewall firewall delete rule name="wifi-cursor" program="$exePath" | Out-Null 2>&1
    netsh advfirewall firewall add rule name="wifi-cursor" dir=in action=allow program="$exePath" enable=yes profile=private,domain | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "Добавлено правило брандмауэра для частных/домашних сетей — входящие подключения для wifi-cursor.exe разрешены (Windows Firewall по умолчанию их блокирует, из-за чего create/join не видят друг друга). На публичных/гостевых Wi-Fi правило не действует — это осознанно." -ForegroundColor Green
    } else {
        Write-Host "Не удалось добавить правило брандмауэра. Выполните вручную:" -ForegroundColor Yellow
        Write-Host "  $fwRuleCmd" -ForegroundColor Yellow
    }
} else {
    Write-Host "Скрипт запущен без прав администратора — не могу добавить правило брандмауэра автоматически." -ForegroundColor Yellow
    Write-Host "Если create/join не находят друг друга по сети, откройте PowerShell от имени администратора и выполните:" -ForegroundColor Yellow
    Write-Host "  $fwRuleCmd" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "Готово! Запуск:" -ForegroundColor Cyan
Write-Host "  wifi-cursor create"
Write-Host "  wifi-cursor join <ID>"
