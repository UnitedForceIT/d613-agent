# D613 Labs Remote Agent — one-liner installer for Windows (PowerShell)
#
# Usage (run this in PowerShell on the machine you want to access remotely):
#   iwr -useb https://github.com/d613labs/d613-agent/releases/latest/download/install.ps1 | iex
#
# The script detects your architecture, downloads the right binary, and starts
# the agent.  No account, no configuration, no installation required.

$ErrorActionPreference = "Stop"

$repo     = "UnitedForceIT/d613-agent"
$binary   = "d613-agent"
$arch     = if ([System.Environment]::Is64BitOperatingSystem) { "amd64" } else { "386" }
$filename = "$binary-windows-$arch.exe"
$url      = "https://github.com/$repo/releases/latest/download/$filename"
$dest     = "$env:TEMP\$filename"

Write-Host ""
Write-Host "  D613 Labs Remote Agent"
Write-Host "  -----------------------------------------"
Write-Host "  Platform : windows/$arch"
Write-Host "  Downloading $filename ..."
Write-Host ""

Invoke-WebRequest -Uri $url -OutFile $dest

Write-Host "  Download complete.  Starting agent..."
Write-Host ""

& $dest @args
