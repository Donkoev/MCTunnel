<#
.SYNOPSIS
  Builds the MCTunnel relay and installs it on a Linux VPS over SSH (Windows PowerShell 5.1+).

.DESCRIPTION
  The same installer as the one-command install on the server (install.sh), but with the relay
  binaries sent from this PC: for servers that cannot reach GitHub, or to deploy your own build.
  1. Builds static Linux binaries (amd64 + arm64) with Go. If mctunnel-relay-linux-amd64 and
     mctunnel-relay-linux-arm64 sit next to this script (a release folder), it uses them
     instead and needs neither Go nor the sources.
  2. Uploads them with install.sh and (optionally) a relay.json to /tmp/mctunnel-deploy.
  3. Runs install.sh with sudo: on a new server it asks its three questions (user name, address,
     ports) and prints the data for the mod; an installed relay is updated (with -Config: gets
     that relay.json). It checks the new binary against the config before replacing anything,
     and puts the previous version back if the relay does not start.
  SSH and sudo passwords (if any) are typed by you in this terminal; the script never stores
  them. With -SetupKey it first adds your SSH public key to the server, so later runs need no
  SSH password. -Identity uses a specific private key file. -BindAddress sends the connection
  from a given local IP, e.g. your physical adapter's address to bypass a VPN/TUN adapter.

.EXAMPLE
  .\deploy.ps1 -Server 203.0.113.10 -User user -Config $env:USERPROFILE\.mctunnel\relay.json -SetupKey
.EXAMPLE
  .\deploy.ps1 -Server 203.0.113.10 -User user -Identity C:\keys\id_rsa -BindAddress 192.168.1.50
.EXAMPLE
  .\deploy.ps1 -Server 203.0.113.10 -User user          # upgrade, keep the server's config
#>
param(
	[Parameter(Mandatory = $true)] [string] $Server,
	[string] $User = "root",
	[int] $Port = 22,
	# relay.json to install; omit on upgrades to keep the one on the server
	[string] $Config = "",
	# private key file to log in with
	[string] $Identity = "",
	# local source IP for SSH (bypass a VPN by using the physical adapter's address)
	[string] $BindAddress = "",
	# add your public key to the server's authorized_keys first: <Identity>.pub with -Identity,
	# else ~/.ssh/id_ed25519.pub (or id_rsa.pub)
	[switch] $SetupKey,
	# version stamped into binaries built from source (prebuilt ones keep their own); default:
	# mod_version from ..\..\mod-fabric\gradle.properties in the source tree, else 1.0.0
	[string] $Version = ""
)
$ErrorActionPreference = "Stop"
$deployDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$serverDir = Split-Path -Parent $deployDir
$target = "$User@$Server"

if ($Config -and -not (Test-Path $Config)) { throw "Config file not found: $Config" }

# Options shared by ssh and scp (the port flag differs: ssh -p, scp -P).
$common = @("-o", "StrictHostKeyChecking=accept-new")
if ($Identity) { $common += @("-i", $Identity, "-o", "IdentitiesOnly=yes") }
if ($BindAddress) { $common += @("-o", "BindAddress=$BindAddress") }
$sshOpts = $common + @("-p", "$Port")
$scpOpts = $common + @("-P", "$Port")

if ($SetupKey) {
	if ($Identity) {
		$pub = "$Identity.pub"
		if (-not (Test-Path $pub)) { throw "Public key not found: $pub (create it with: ssh-keygen -y -f $Identity | Set-Content -Encoding ascii $pub)" }
	} else {
		$pub = @("$env:USERPROFILE\.ssh\id_ed25519.pub", "$env:USERPROFILE\.ssh\id_rsa.pub") | Where-Object { Test-Path $_ } | Select-Object -First 1
		if (-not $pub) { throw "No SSH public key found; create one with: ssh-keygen -t ed25519" }
	}
	Write-Host "==> Adding $pub to $target (enter the SSH password once)"
	# On the server: end authorized_keys with a newline first (else the key would be glued to
	# the last line), and skip a key that is already there. No double quotes: Windows
	# PowerShell mangles them in arguments to native programs.
	$remote = 'umask 077; mkdir -p ~/.ssh; f=~/.ssh/authorized_keys; t=$(mktemp); tr -d ''\r'' | awk NF > $t; ' +
		'if [ -s $f ] && [ $(tail -c1 $f | wc -l) -eq 0 ]; then echo >> $f; fi; ' +
		'if grep -qxFf $t $f 2>/dev/null; then echo key already present; else cat $t >> $f; fi; rc=$?; rm -f $t; exit $rc'
	Get-Content $pub | ssh @sshOpts $target $remote
	if ($LASTEXITCODE -ne 0) { throw "ssh failed" }
}

$stage = Join-Path $env:TEMP "mctunnel-deploy"
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory $stage | Out-Null

# A release folder ships the binaries next to this script; the source tree builds them.
$prebuilt = @("amd64", "arm64") | ForEach-Object { Join-Path $deployDir "mctunnel-relay-linux-$_" }
$missing = @($prebuilt | Where-Object { -not (Test-Path $_) })
if ($missing.Count -eq 0) {
	Write-Host "==> Using the prebuilt relay binaries next to deploy.ps1 (delete them to build from source)"
	Copy-Item $prebuilt $stage
} else {
	if (-not (Test-Path (Join-Path $serverDir "go.mod"))) {
		throw "Missing $($missing -join ', ') and no Go sources in $serverDir to build them from"
	}
	if (-not $Version) {
		# The same version as the mod and build-release.ps1, so the relay's log says which build runs.
		$Version = "1.0.0"
		$gradleProps = Join-Path $deployDir "..\..\mod-fabric\gradle.properties"
		if (Test-Path $gradleProps) {
			foreach ($line in Get-Content $gradleProps) {
				if ($line -match '^\s*mod_version\s*=\s*(.+?)\s*$') { $Version = $Matches[1] }
			}
		}
	}
	Write-Host "==> Building relay $Version"
	Push-Location $serverDir
	try {
		foreach ($arch in @("amd64", "arm64")) {
			$env:GOOS = "linux"; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
			go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $stage "mctunnel-relay-linux-$arch") ./cmd/mctunnel-relay
			if ($LASTEXITCODE -ne 0) { throw "go build failed" }
		}
	} finally {
		Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
		Pop-Location
	}
}
Copy-Item (Join-Path $deployDir "install.sh") $stage
if ($Config) { Copy-Item $Config (Join-Path $stage "relay.json") }

# The stage may hold relay.json with the users' secrets: private on the server, removed here
# even when a step fails.
try {
	Write-Host "==> Uploading to ${target}:/tmp/mctunnel-deploy"
	ssh @sshOpts $target "rm -rf /tmp/mctunnel-deploy && mkdir -m 700 /tmp/mctunnel-deploy"
	if ($LASTEXITCODE -ne 0) { throw "ssh failed" }
	scp @scpOpts -r $stage "${target}:/tmp/"
	if ($LASTEXITCODE -ne 0) { throw "scp failed" }

	Write-Host "==> Installing (sudo may ask for your password)"
	ssh -t @sshOpts $target "sudo bash /tmp/mctunnel-deploy/install.sh auto; rc=`$?; rm -rf /tmp/mctunnel-deploy; exit `$rc"
	if ($LASTEXITCODE -ne 0) { throw "install failed" }
} finally {
	Remove-Item -Recurse -Force $stage -ErrorAction SilentlyContinue
}
Write-Host "==> Done"
