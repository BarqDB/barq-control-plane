param(
  [Parameter(Mandatory = $true)]
  [ValidatePattern('^v[0-9][0-9A-Za-z._-]*$')]
  [string]$Version
)

$ErrorActionPreference = 'Stop'
if ($env:PROCESSOR_ARCHITECTURE -notin @('AMD64', 'x86_64')) {
  throw 'Only Windows amd64 is supported.'
}

$Archive = "barq-$Version-windows-amd64.zip"
$Base = if ($env:BARQ_RELEASE_BASE_URL) { $env:BARQ_RELEASE_BASE_URL } else { "https://github.com/BarqDB/barq-control-plane/releases/download/$Version" }
$InstallDir = if ($env:BARQ_INSTALL_DIR) { $env:BARQ_INSTALL_DIR } else { Join-Path $HOME '.barq\bin' }
$Temporary = Join-Path ([IO.Path]::GetTempPath()) ("barq-install-" + [guid]::NewGuid())

try {
  New-Item -ItemType Directory -Force -Path $Temporary | Out-Null
  $ArchivePath = Join-Path $Temporary $Archive
  $ChecksumPath = "$ArchivePath.sha256"
  Invoke-WebRequest -UseBasicParsing -Uri "$Base/$Archive" -OutFile $ArchivePath
  Invoke-WebRequest -UseBasicParsing -Uri "$Base/$Archive.sha256" -OutFile $ChecksumPath
  $Expected = ((Get-Content $ChecksumPath -TotalCount 1) -split '\s+')[0].ToLowerInvariant()
  $Actual = (Get-FileHash -Algorithm SHA256 $ArchivePath).Hash.ToLowerInvariant()
  if (-not $Expected -or $Actual -ne $Expected) {
    throw 'Release checksum does not match.'
  }

  $Package = Join-Path $Temporary 'package'
  Expand-Archive -Path $ArchivePath -DestinationPath $Package -Force
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  foreach ($Program in @('barqctl.exe', 'restic.exe', 'cosign.exe')) {
    Copy-Item -Force (Join-Path $Package $Program) (Join-Path $InstallDir $Program)
  }
  Write-Host "Barq $Version installed in $InstallDir"
  if (($env:PATH -split ';') -notcontains $InstallDir) {
    Write-Host "Add $InstallDir to PATH, then run: barqctl init --domain db.example.com"
  }
}
finally {
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $Temporary
}
