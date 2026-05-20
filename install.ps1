$ErrorActionPreference = "Stop"

$Repo = if ($env:AGENT_BRIDGE_REPO) { $env:AGENT_BRIDGE_REPO } else { "BlueYeoul/agent-bridge" }
$Version = if ($env:AGENT_BRIDGE_VERSION) { $env:AGENT_BRIDGE_VERSION } else { "latest" }
$InstallDir = if ($env:AGENT_BRIDGE_INSTALL_DIR) { $env:AGENT_BRIDGE_INSTALL_DIR } else { Join-Path $HOME ".local\bin" }

$Arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()) {
  "x64" { "amd64" }
  "arm64" { "arm64" }
  default { throw "Unsupported architecture: $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)" }
}

$Archive = "agent-bridge_windows_$Arch.zip"
if ($Version -eq "latest") {
  $Url = "https://github.com/$Repo/releases/latest/download/$Archive"
} else {
  $Url = "https://github.com/$Repo/releases/download/$Version/$Archive"
}

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) "agent-bridge-install-$([System.Guid]::NewGuid())"
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null

try {
  $ArchivePath = Join-Path $TempDir $Archive
  Write-Host "agent-bridge installer: downloading $Url"
  Invoke-WebRequest -Uri $Url -OutFile $ArchivePath

  Expand-Archive -Path $ArchivePath -DestinationPath $TempDir -Force
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

  $BinaryPath = Join-Path $InstallDir "agent-bridge.exe"
  Copy-Item -Path (Join-Path $TempDir "agent-bridge.exe") -Destination $BinaryPath -Force

  $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
  $PathParts = @()
  if ($UserPath) {
    $PathParts = $UserPath -split ";"
  }
  if ($PathParts -notcontains $InstallDir) {
    $NextPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("Path", $NextPath, "User")
    $env:Path = "$env:Path;$InstallDir"
    Write-Host "agent-bridge installer: added $InstallDir to your user PATH"
  }

  & $BinaryPath --help | Out-Null
  Write-Host "agent-bridge installer: installed $BinaryPath"
  Write-Host "agent-bridge installer: ready"
} finally {
  Remove-Item -Recurse -Force $TempDir -ErrorAction SilentlyContinue
}
