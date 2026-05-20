$ErrorActionPreference = "Stop"

$Repo = if ($env:AGENT_BRIDGE_REPO) { $env:AGENT_BRIDGE_REPO } else { "BlueYeoul/agent-bridge" }
$Version = if ($env:AGENT_BRIDGE_VERSION) { $env:AGENT_BRIDGE_VERSION } else { "latest" }
$InstallDir = if ($env:AGENT_BRIDGE_INSTALL_DIR) { $env:AGENT_BRIDGE_INSTALL_DIR } else { Join-Path $HOME ".local\bin" }
$ModulePath = "github.com/$Repo/cmd/agent-bridge"

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
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  $BinaryPath = Join-Path $InstallDir "agent-bridge.exe"

  try {
    Invoke-WebRequest -Uri $Url -OutFile $ArchivePath
    Expand-Archive -Path $ArchivePath -DestinationPath $TempDir -Force
    Copy-Item -Path (Join-Path $TempDir "agent-bridge.exe") -Destination $BinaryPath -Force
  } catch {
    Write-Host "agent-bridge installer: release binary unavailable; falling back to source install"
    $Go = Get-Command go -ErrorAction SilentlyContinue
    if (-not $Go) {
      throw "release binary was not available and Go is not installed. Publish a GitHub Release or install Go, then retry."
    }
    $OldGoBin = $env:GOBIN
    try {
      $env:GOBIN = $InstallDir
      & go install "$ModulePath@$Version"
      if ($LASTEXITCODE -ne 0) {
        throw "go install failed with exit code $LASTEXITCODE"
      }
    } finally {
      $env:GOBIN = $OldGoBin
    }
  }

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
