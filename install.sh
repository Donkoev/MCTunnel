#!/usr/bin/env bash
# MCTunnel relay installer for a Linux VPS with systemd (Ubuntu, Debian and the like).
#
# One command on the server:
#   curl -fsSL https://github.com/Donkoev/MCTunnel/releases/latest/download/install.sh | sudo bash
#
# The first run asks three questions (a user name, the server's address, the ports), downloads
# the relay of this release, checks its SHA-256, installs it as the systemd service
# mctunnel-relay, turns on a firewall that leaves only the relay and SSH open (ufw; what else
# listens is shown first, and can be kept) and prints what to enter in the mod. Every later run
# opens a menu: update the relay, show a user's data for the mod, add or remove users, restart,
# the log, the firewall, uninstall.
#
# Without a terminal, or in scripts, give the action as arguments (after a pipe:
# `| sudo bash -s -- ACTION ...`):
#   install [--user NAME] [--address ADDR] [--ports "TCP TLS KCP MIN-MAX"]  (0 = no TLS or KCP)
#           [--keep "80 443/tcp"] [--no-firewall]
#   update                  a new relay binary, unit and sysctl file; the configuration stays
#   list | add NAME | show NAME | remove NAME
#   firewall [--keep "80 443/tcp"]  only the relay, SSH and --keep open from the outside
#   uninstall --yes
#   auto                    what deploy.ps1 runs: a relay.json next to the script is installed,
#                           an installed relay is updated, a new server gets the questions
# Relay binaries next to the script (deploy.ps1, a release folder) are used instead of a
# download. MCTUNNEL_VERSION picks another release, NO_COLOR turns the colors off, and
# MCTUNNEL_NAME renames the service, user and paths (a second copy beside the real one, for
# tests).
#
# Nothing is replaced before the new binary has accepted the configuration it will run with.
# An update keeps the previous binary as /usr/local/bin/mctunnel-relay.prev and puts it (and a
# replaced configuration or unit) back if the relay does not stay up.

set -Eeuo pipefail

REPO="${MCTUNNEL_REPO:-Donkoev/MCTunnel}"
RELEASE="dev" # build-release.ps1 stamps the release version here
VERSION="${MCTUNNEL_VERSION:-$RELEASE}"
NAME="${MCTUNNEL_NAME:-mctunnel}"

SERVICE="$NAME-relay"
SYSUSER="$NAME"
BIN="/usr/local/bin/$NAME-relay"
ETC="/etc/$NAME"
CONF="$ETC/relay.json"
STATE="$ETC/installer.env"
UNIT="/etc/systemd/system/$SERVICE.service"
SYSCTL="/etc/sysctl.d/99-$NAME-bbr.conf"
MODLOAD="/etc/modules-load.d/$NAME-bbr.conf"

HERE=""       # the script's directory when it runs from a file; empty after a pipe
WORK=""       # temporary directory, removed on exit
TTY=""        # where answers are read from; empty: no terminal, defaults are taken
ARCH=""
ACTION=""
ACTION_ARG=""
OPT_USER=""
OPT_ADDRESS=""
OPT_PORTS=""
OPT_YES=0
OPT_KEEP=""        # --keep "80 443/tcp": ports the firewall leaves open besides the relay and SSH
OPT_NO_FIREWALL=0  # --no-firewall: the first installation leaves the firewall alone
FIREWALL_KEEP=()
NEW_CONFIG=""  # a relay.json to install instead of the one in $CONF (deploy.ps1 -Config)
PICKED=""      # set by pick_user

# ── output ──────────────────────────────────────────────────────────────────────────────────

setup_output() {
	if [[ -t 1 && -z ${NO_COLOR:-} && ${TERM:-dumb} != dumb ]]; then
		COLOR=1
		R=$'\e[0m' BOLD=$'\e[1m'
		# The Minecraft chat palette in 256 colors: §6 gold, §e yellow, §a green, §2 dark
		# green, §b aqua, §c red, §7 gray, §8 dark gray, §f white.
		GOLD=$'\e[38;5;214m' YELLOW=$'\e[38;5;227m' GREEN=$'\e[38;5;83m' DGREEN=$'\e[38;5;34m'
		AQUA=$'\e[38;5;87m' RED=$'\e[38;5;203m' GRAY=$'\e[38;5;248m' DGRAY=$'\e[38;5;242m'
		WHITE=$'\e[38;5;231m'
	else
		COLOR=0
		R="" BOLD="" GOLD="" YELLOW="" GREEN="" DGREEN="" AQUA="" RED="" GRAY="" DGRAY="" WHITE=""
	fi
}

title() { printf '\n  %s%s■ %s%s\n' "$GOLD" "$BOLD" "$1" "$R"; }
ok() { printf '  %s✔%s %s\n' "$GREEN" "$R" "$1"; }
info() { printf '  %s•%s %s\n' "$GRAY" "$R" "$1"; }
warn() { printf '  %s!%s %s%s%s\n' "$YELLOW$BOLD" "$R" "$YELLOW" "$1" "$R"; }
hint() { printf '    %s%s%s\n' "$GRAY" "$1" "$R"; }

# die MESSAGE [DETAIL...]: the reason in red, then the details, and exit.
die() {
	printf '\n  %s✘ %s%s\n' "$RED$BOLD" "$1" "$R" >&2
	shift
	local line
	for line in "$@"; do printf '    %s\n' "$line" >&2; done
	printf '\n' >&2
	exit 1
}

# width TEXT: how many columns TEXT takes (colors dropped, UTF-8 counted as characters, in any
# locale).
width() {
	local s
	s=$(printf '%s' "$1" | sed $'s/\e\\[[0-9;]*m//g')
	printf '%s' "$s" | LC_ALL=C tr -d '\200-\277' | wc -c | tr -d ' '
}

# pad TEXT COLUMNS: TEXT and spaces up to COLUMNS.
pad() {
	local n
	n=$(width "$1")
	printf '%s%*s' "$1" $(($2 > n ? $2 - n : 0)) ''
}

# box COLOR LINE...: a frame around the lines (which may hold colors).
box() {
	local color=$1 w=0 n line bar
	shift
	for line in "$@"; do
		n=$(width "$line")
		if ((n > w)); then w=$n; fi
	done
	bar=$(printf '%*s' $((w + 2)) '' | sed 's/ /─/g')
	printf '  %s╭%s╮%s\n' "$color" "$bar" "$R"
	for line in "$@"; do
		printf '  %s│%s %s %s│%s\n' "$color" "$R" "$(pad "$line" "$w")" "$color" "$R"
	done
	printf '  %s╰%s╯%s\n' "$color" "$bar" "$R"
}

# The side of a grass block, 16×16, two pixel rows per text line (▀: the upper pixel is the
# text color, the lower one the background).
GRASS=(
	"gGgGGgGggGgGGgGg" "GgkgGGgkGgGgkGGg" "gkGgkgGgkgGkgGkg" "kgDkgDgkDgkgDkgD"
	"DkDDgDDkDDkDDgDD" "DDLDDdDDLDDDdDLD" "LDDDDDDLDDdDDDDD" "DdDLDDDDDDDDLDDd"
	"DDDDDdDDLDdDDDDD" "DLDDDDDDDDDDDdLD" "DDDdDLDDdDDLDDDD" "LDDDDDDDDDDDDDDL"
	"DDLDDdDDDLDDdDDD" "DDDDDDLDDDDDDDLD" "DdDDLDDDdDDLDDDD" "DDDDDDDdDDDDDdDD"
)
SPLASHES=(
	"Без белого IP!" "Работает за CGNAT!" "Друзьям мод не нужен!" "Теперь с KCP!"
	"Копаем туннель…" "Свой сервер — свои правила!" "Секрет не покидает ПК!"
)

banner() {
	local version=$RELEASE
	[[ $version == dev ]] && version="из исходников"
	if ((COLOR == 0)); then
		printf '\n  MCTunnel · relay-сервер для вашего мира Minecraft · %s\n' "$version"
		return
	fi
	local -A pixel=([G]=107 [g]=71 [k]=64 [D]=94 [d]=58 [L]=137)
	local splash=${SPLASHES[RANDOM % ${#SPLASHES[@]}]}
	local text=(
		""
		"${WHITE}${BOLD}MCTunnel${R}  ${DGRAY}установщик relay · ${version}${R}"
		"${GRAY}Ваш мир Minecraft — друзьям, через свой VPS.${R}"
		""
		"${YELLOW}${BOLD}${splash}${R}"
		"" "" ""
	)
	local i j top bottom line
	printf '\n'
	for ((i = 0; i < 8; i++)); do
		top=${GRASS[2 * i]} bottom=${GRASS[2 * i + 1]} line="  "
		for ((j = 0; j < 16; j++)); do
			line+=$'\e[38;5;'"${pixel[${top:j:1}]}"$'m\e[48;5;'"${pixel[${bottom:j:1}]}"'m▀'
		done
		printf '%s%s   %s\n' "$line" "$R" "${text[i]}"
	done
}

# The advancement toast of the game, for a finished installation.
toast() {
	printf '\n'
	box "$DGRAY" "${YELLOW}★ Достижение получено!${R}" "  ${WHITE}$1${R}"
}

# ── input ───────────────────────────────────────────────────────────────────────────────────

setup_input() {
	if [[ -t 0 ]]; then
		TTY=/dev/stdin
	elif { : </dev/tty; } 2>/dev/null; then
		TTY=/dev/tty # piped from curl: the answers come from the terminal
	fi
}

interactive() { [[ -n $TTY ]]; }

# ask VAR QUESTION [DEFAULT]: one line of input; Enter takes the default.
ask() {
	local __var=$1 __question=$2 __default=${3:-} __answer=""
	if ! interactive; then
		printf -v "$__var" '%s' "$__default"
		return 0
	fi
	printf '  %s?%s %s' "$AQUA$BOLD" "$R" "$__question"
	[[ -n $__default ]] && printf ' %s[%s]%s' "$DGRAY" "$__default" "$R"
	printf ': '
	IFS= read -r __answer <"$TTY" || true
	__answer=${__answer%$'\r'}
	__answer=${__answer#"${__answer%%[![:space:]]*}"}
	__answer=${__answer%"${__answer##*[![:space:]]}"}
	printf -v "$__var" '%s' "${__answer:-$__default}"
}

# confirm QUESTION [y|n]: yes or no; Enter takes the default. Both keyboard layouts work: д, да,
# y, yes (and l, the д key on a Latin layout) mean yes.
confirm() {
	local answer default=${2:-y}
	if ! interactive; then
		[[ $default == y ]]
		return
	fi
	while :; do
		ask answer "$1 $([[ $default == y ]] && printf '%s(Д/н)%s' "$DGRAY" "$R" || printf '%s(д/Н)%s' "$DGRAY" "$R")" ""
		case "$answer" in
			"") [[ $default == y ]]; return ;;
			д | Д | да | Да | ДА | l | L | y | Y | yes | Yes | YES) return 0 ;;
			н | Н | нет | Нет | НЕТ | n | N | no | No | NO) return 1 ;;
			*) warn "Ответьте «д» (да) или «н» (нет)." ;;
		esac
	done
}

valid_name() { [[ $1 =~ ^[A-Za-z0-9._-]{1,32}$ ]]; }
valid_address() { [[ $1 =~ ^[A-Za-z0-9.:-]{1,253}$ ]]; }
valid_port() { [[ $1 =~ ^[0-9]{1,5}$ ]] && (($1 >= 1 && $1 <= 65535)); }

# ── system ──────────────────────────────────────────────────────────────────────────────────

check_system() {
	[[ $(uname -s) == Linux ]] || die "Нужен сервер на Linux."
	if ((EUID != 0)); then
		die "Нужны права root." \
			"Запустите так: curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sudo bash"
	fi
	if ! command -v systemctl >/dev/null 2>&1 || [[ ! -d /run/systemd/system ]]; then
		die "Нужен systemd (Ubuntu, Debian и похожие системы)."
	fi
	case "$(uname -m)" in
		x86_64 | amd64) ARCH=amd64 ;;
		aarch64 | arm64) ARCH=arm64 ;;
		*) die "Процессор $(uname -m) не поддерживается: нужен x86_64 или arm64." ;;
	esac
}

cleanup() {
	rm -f "$BIN.new" "$UNIT.new" "$CONF.new"
	[[ -n $WORK ]] && rm -rf "$WORK"
	return 0
}

on_error() {
	printf '\n  %s✘ Что-то пошло не так (строка %s: %s).%s\n' "$RED$BOLD" "$1" "$2" "$R" >&2
	printf '    Запустите установщик ещё раз; если не поможет, пришлите этот вывод:\n' >&2
	printf '    https://github.com/%s/issues\n\n' "$REPO" >&2
}

installed() { [[ -f $CONF ]]; }

require_installed() {
	installed || die "MCTunnel relay здесь не установлен ($CONF нет)." "Запустите установщик без аргументов."
}

relay_version() { "$1" version 2>/dev/null | awk '{print $2}'; }

# manageable: the installed relay has the user commands and -print-listen: every release (1.0.0
# is the first) and a build from source ("dev"). Anything else could take `user list` for its own
# arguments and start a second relay.
manageable() {
	local v
	[[ -x $BIN ]] || return 1
	v=$(relay_version "$BIN")
	[[ $v == dev ]] || [[ $(printf '%s\n' 1.0.0 "$v" | sort -V | head -n 1) == 1.0.0 ]]
}

require_manageable() {
	manageable && return 0
	warn "Установленный relay ${1:-}слишком старый для этого: сначала обновите его (пункт 1 меню)."
	return 1
}

# fetch URL FILE
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 20 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 20 -t 3 -O "$2" "$1"
	else
		die "Нужен curl или wget." "Установите: apt install curl"
	fi
}

# fetch_text URL: a short answer from a web service (IPv4, 5 s at most).
fetch_text() {
	if command -v curl >/dev/null 2>&1; then
		curl -4 -fsS --max-time 5 "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -4 -q -T 5 -O - "$1"
	fi
}

release_url() {
	if [[ $VERSION == dev || $VERSION == latest ]]; then
		printf 'https://github.com/%s/releases/latest/download/%s' "$REPO" "$1"
	else
		printf 'https://github.com/%s/releases/download/v%s/%s' "$REPO" "${VERSION#v}" "$1"
	fi
}

# stage_binary: the new relay binary at $BIN.new, from next to the script or from the release.
stage_binary() {
	local asset="mctunnel-relay-linux-$ARCH" want got
	if [[ -n $HERE && -f $HERE/$asset ]]; then
		install -m 0755 "$HERE/$asset" "$BIN.new"
		ok "relay $(relay_version "$BIN.new") ($ARCH) из $HERE"
		return
	fi
	info "Скачиваю relay с GitHub ($REPO)…"
	fetch "$(release_url "$asset")" "$WORK/$asset" ||
		die "Не удалось скачать relay." "$(release_url "$asset")" "Проверьте, что сервер открывает github.com."
	fetch "$(release_url SHA256SUMS.txt)" "$WORK/SHA256SUMS.txt" ||
		die "Не удалось скачать контрольные суммы." "$(release_url SHA256SUMS.txt)"
	# The list may come with Windows line endings; names may carry a folder ("relay/...").
	want=$(tr -d '\r' <"$WORK/SHA256SUMS.txt" | awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n); sub(/.*\//, "", n); if (n == a) print $1 }' | head -n 1)
	got=$(sha256sum "$WORK/$asset" | awk '{print $1}')
	if [[ -z $want || $want != "$got" ]]; then
		die "Контрольная сумма relay не совпала: файл повреждён или подменён." "Ничего не установлено."
	fi
	install -m 0755 "$WORK/$asset" "$BIN.new"
	ok "скачан relay $(relay_version "$BIN.new") ($ARCH), SHA-256 совпал"
}

ensure_sysuser() {
	if ! id "$SYSUSER" >/dev/null 2>&1; then
		useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$SYSUSER"
		ok "системный пользователь $SYSUSER (relay работает без прав root)"
	fi
	install -d -m 0750 -o root -g "$SYSUSER" "$ETC"
}

saved_address() {
	[[ -f $STATE ]] && sed -n 's/^ADDRESS=//p' "$STATE" | head -n 1
	return 0
}

save_address() {
	printf 'ADDRESS=%s\n' "$1" >"$STATE"
	chmod 0644 "$STATE"
}

# detect_ip: this server's public IPv4 address (as the internet sees it), or nothing.
detect_ip() {
	local url ip
	for url in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com; do
		ip=$(fetch_text "$url" 2>/dev/null | tr -d ' \r\n' || true)
		if [[ $ip =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
			printf '%s' "$ip"
			return
		fi
	done
	ip -4 -o addr show scope global 2>/dev/null | awk '{sub(/\/.*/, "", $4); print $4; exit}'
}

# listening PROTO: the ports something listens on (tcp or udp), one per line.
listening() {
	command -v ss >/dev/null 2>&1 || return 0
	ss -H -ln --"$1" 2>/dev/null | awk '{n = split($4, a, ":"); print a[n]}' | sort -u
}

write_sysctl() {
	cat >"$SYSCTL" <<'EOF'
# Installed by the MCTunnel installer.
# BBR congestion control + fq qdisc: better throughput and latency on lossy or long paths,
# which is exactly what players on mobile or CGNAT links have.
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr

# The KCP listener is one UDP socket for every host and asks for 4 MiB buffers; Linux silently
# caps that at these limits (about 208 KiB by default), and a full buffer drops datagrams.
net.core.rmem_max = 4194304
net.core.wmem_max = 4194304
EOF
	echo tcp_bbr >"$MODLOAD"
	modprobe tcp_bbr 2>/dev/null || true
	sysctl -q -p "$SYSCTL" >/dev/null 2>&1 || true
	if [[ $(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null) == bbr ]]; then
		ok "BBR включён, буферы UDP увеличены"
	else
		info "BBR на этом ядре недоступен: relay работает и без него"
	fi
}

write_unit() {
	cat >"$UNIT.new" <<EOF
[Unit]
Description=MCTunnel relay (Minecraft LAN tunnel)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SYSUSER
Group=$SYSUSER
ExecStart=$BIN -config $CONF
# \`systemctl reload $SERVICE\` re-reads the user list: add or revoke users without
# disconnecting anyone else.
ExecReload=/bin/kill -HUP \$MAINPID
Restart=on-failure
RestartSec=2s
LimitNOFILE=65536

# Hardening: the relay opens TCP sockets and one UDP socket (the KCP listener). Its one
# privilege is binding ports below 1024 (the TLS listener on 443); nothing else.
NoNewPrivileges=yes
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
# Other processes are invisible and /proc holds only process entries. Without
# /proc/sys/net/core/somaxconn, Go uses a listen backlog of 128: plenty for this relay.
ProtectProc=invisible
ProcSubset=pid
PrivateIPC=yes
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
}

# ports_of: the host and player ports of the installed configuration, for the texts
# (TCP_PORTS, UDP_PORTS, PLAYER_RANGE) and the firewall (HOST_PORTS: "25500/tcp" ...).
ports_of() {
	local line
	HOST_PORTS=() TCP_PORTS=() UDP_PORTS=() PLAYER_RANGE=""
	while IFS= read -r line; do
		case "$line" in
			"hosts "*)
				HOST_PORTS+=("${line#hosts }")
				case "$line" in
					*/tcp) TCP_PORTS+=("${line#hosts }") ;;
					*/udp) UDP_PORTS+=("${line#hosts }") ;;
				esac
				;;
			"players "*) PLAYER_RANGE=${line#players } ;;
		esac
	done < <("$BIN" -config "$CONF" -print-ports)
	TCP_PORTS=("${TCP_PORTS[@]%/tcp}")
	UDP_PORTS=("${UDP_PORTS[@]%/udp}")
}

# listeners: LISTEN_TCP, LISTEN_TLS, LISTEN_KCP of the installed configuration ("" if none).
listeners() {
	local transport port
	LISTEN_TCP="" LISTEN_TLS="" LISTEN_KCP=""
	while read -r transport port; do
		case "$transport" in
			tcp) LISTEN_TCP=$port ;;
			tls) LISTEN_TLS=$port ;;
			kcp) LISTEN_KCP=$port ;;
		esac
	done < <("$BIN" -config "$CONF" -print-listen)
}

# ports_text: "TCP 25500, 443, 25565–25664 и UDP 25500"
ports_text() {
	local tcp
	tcp=$(printf '%s, ' "${TCP_PORTS[@]}" "${PLAYER_RANGE/-/–}")
	tcp=${tcp%, }
	if ((${#UDP_PORTS[@]} > 0)); then
		printf 'TCP %s и UDP %s' "$tcp" "$(printf '%s, ' "${UDP_PORTS[@]}" | sed 's/, $//')"
	else
		printf 'TCP %s' "$tcp"
	fi
}

open_firewall() {
	local p
	ports_of
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		for p in "${HOST_PORTS[@]}"; do ufw allow "$p" comment "$NAME hosts" >/dev/null; done
		ufw allow "${PLAYER_RANGE/-/:}/tcp" comment "$NAME players" >/dev/null
		ok "ufw: открыты $(ports_text)"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		for p in "${HOST_PORTS[@]}"; do firewall-cmd -q --permanent --add-port="$p"; done
		firewall-cmd -q --permanent --add-port="$PLAYER_RANGE/tcp"
		firewall-cmd -q --reload
		ok "firewalld: открыты $(ports_text)"
	else
		info "файрвола на сервере нет (ufw и firewalld выключены)"
	fi
}

close_firewall() {
	local p
	ports_of 2>/dev/null || return 0
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		for p in "${HOST_PORTS[@]}"; do ufw delete allow "$p" >/dev/null 2>&1 || true; done
		ufw delete allow "${PLAYER_RANGE/-/:}/tcp" >/dev/null 2>&1 || true
		ok "ufw: порты закрыты"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		for p in "${HOST_PORTS[@]}"; do firewall-cmd -q --permanent --remove-port="$p" || true; done
		firewall-cmd -q --permanent --remove-port="$PLAYER_RANGE/tcp" || true
		firewall-cmd -q --reload || true
		ok "firewalld: порты закрыты"
	fi
}

# ── firewall: from the outside only the relay, SSH and what the system itself needs ─────────

# ssh_ports: the TCP ports SSH is reachable on, one per line: the listening sockets of sshd (or
# dropbear), its systemd socket, its configuration, and the port of every open SSH session.
# Empty when there is no SSH: the firewall is then left alone, it could lock the owner out.
ssh_ports() {
	{
		ss -H -ltnp 2>/dev/null | awk '/"(sshd|dropbear)"/ {n = split($4, a, ":"); print a[n]}'
		systemctl show -p Listen ssh.socket sshd.socket 2>/dev/null | sed -n 's/^Listen=.*:\([0-9][0-9]*\) (Stream)$/\1/p'
		if command -v sshd >/dev/null 2>&1; then sshd -T 2>/dev/null | awk '$1 == "port" {print $2}'; fi
		ss -H -tnp state established 2>/dev/null | awk '/"(sshd|dropbear)"/ {n = split($3, a, ":"); print a[n]}'
	} | grep -E '^[0-9]+$' | sort -un
}

# other_listeners SKIP...: what else accepts connections from the outside and would be closed,
# as "tcp 80 nginx" lines. SKIP are ports that stay open ("25500/tcp", "25565-25664/tcp");
# loopback addresses and the system's DHCP client are not counted.
other_listeners() {
	{
		ss -H -ltnp 2>/dev/null | awk '{print "tcp", $4, $6}'
		ss -H -lunp 2>/dev/null | awk '{print "udp", $4, $6}'
	} | awk -v skip="$*" '
		BEGIN {
			n = split(skip, s, " ")
			for (i = 1; i <= n; i++) {
				split(s[i], pp, "/"); split(pp[1], r, "-")
				lo[i] = r[1] + 0; hi[i] = (r[2] == "" ? r[1] : r[2]) + 0; pr[i] = pp[2]
			}
		}
		{
			k = split($2, a, ":"); port = a[k] + 0
			if ($2 ~ /^127\./ || $2 ~ /^\[::1\]/ || $2 ~ /%lo:/) next
			if ($1 == "udp" && (port == 68 || port == 546)) next
			for (i = 1; i <= n; i++) if ((pr[i] == "" || pr[i] == $1) && port >= lo[i] && port <= hi[i]) next
			proc = $3; sub(/^users:\(\("/, "", proc); sub(/".*/, "", proc)
			print $1, port, proc
		}' | sort -u -k1,1 -k2,2n
}

# valid_keep PORT: a port to keep open for ufw: "80", "80/tcp", "8000:8010/udp" (a range needs
# its protocol).
valid_keep() {
	[[ $1 =~ ^([0-9]{1,5})(:([0-9]{1,5}))?(/(tcp|udp))?$ ]] || return 1
	((BASH_REMATCH[1] >= 1 && BASH_REMATCH[1] <= 65535)) || return 1
	[[ -z ${BASH_REMATCH[2]} ]] || { ((BASH_REMATCH[3] <= 65535)) && [[ -n ${BASH_REMATCH[4]} ]]; }
}

# firewall_step OPEN...: tells what the firewall will close (everything but OPEN, "25500/tcp",
# and SSH) and asks which of those ports to keep (FIREWALL_KEEP). Returns 1 when the owner does
# not want the firewall.
firewall_step() {
	local others=() line proto port proc answer p bad ssh
	FIREWALL_KEEP=()
	((OPT_NO_FIREWALL == 0)) || return 1
	ssh=$(ssh_ports | tr '\n' ' ')
	ssh=${ssh% }
	mapfile -t others < <(other_listeners "$@" $(ssh_ports | sed 's|$|/tcp|'))
	hint "Файрвол: снаружи останутся открыты только порты relay и SSH (${ssh:-не найден}),"
	hint "все остальные закроются. Исходящие соединения, ping и системные службы работают как раньше."
	if ((${#others[@]} > 0)); then
		warn "Сейчас снаружи доступны и закроются:"
		for line in "${others[@]}"; do
			read -r proto port proc <<<"$line"
			hint "  ${proto^^} $port ($proc)"
		done
		if [[ -n $OPT_KEEP ]]; then
			for p in $OPT_KEEP; do
				valid_keep "$p" || die "--keep: «$p» — не порт (80, 80/tcp или 8000:8010/tcp)."
				FIREWALL_KEEP+=("$p")
			done
		elif interactive; then
			while :; do
				ask answer "Оставить какие-то открытыми? Номера через пробел, например 80 443/tcp (Enter — нет)" ""
				bad="" FIREWALL_KEEP=()
				for p in $answer; do
					if valid_keep "$p"; then FIREWALL_KEEP+=("$p"); else bad+=" $p"; fi
				done
				[[ -n $bad ]] || break
				warn "Не понял:$bad. Порт — число (можно с /tcp или /udp), диапазон — 8000:8010/tcp."
			done
		fi
	fi
	confirm "Включить файрвол?" y
}

# lockdown KEEP...: turns the firewall (ufw) on so that only the relay, SSH and KEEP are open to
# the outside. Outgoing connections and the answers to them, loopback, ping and DHCP keep working
# (ufw's own rules). The old ufw rules are backed up by `ufw reset`.
lockdown() {
	local ssh=() p kept=""
	mapfile -t ssh < <(ssh_ports)
	if ((${#ssh[@]} == 0)); then
		warn "Не нашёл, на каком порту работает SSH: файрвол не трогаю, чтобы не потерять доступ к серверу."
		return 1
	fi
	if ! command -v ufw >/dev/null 2>&1; then
		if ! command -v apt-get >/dev/null 2>&1; then
			warn "Здесь нет ufw: закройте лишние порты файрволом своей системы."
			return 1
		fi
		info "Ставлю файрвол ufw…"
		if ! DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ufw >/dev/null 2>&1; then
			warn "Не удалось поставить ufw: закройте лишние порты сами."
			return 1
		fi
	fi
	ports_of
	ufw --force reset >/dev/null 2>&1 || true
	ufw default deny incoming >/dev/null
	ufw default allow outgoing >/dev/null
	for p in "${ssh[@]}"; do ufw allow "$p/tcp" comment "ssh" >/dev/null; done
	for p in "${HOST_PORTS[@]}"; do ufw allow "$p" comment "$NAME hosts" >/dev/null; done
	ufw allow "${PLAYER_RANGE/-/:}/tcp" comment "$NAME players" >/dev/null
	for p in "$@"; do ufw allow "$p" comment "kept open by the $NAME installer" >/dev/null; done
	ufw --force enable >/dev/null
	(($# == 0)) || kept=" и $*"
	ok "файрвол включён: снаружи открыты только SSH (${ssh[*]}), relay$kept"
	hint "Остальные порты закрыты. Прежние правила ufw сохранены в /etc/ufw/*.rules.<время>."
}

# relay_up: the relay runs and is still the same process a few seconds later (one that exits
# at once is restarted by systemd every 2 s, with a new PID each time).
relay_up() {
	local pid
	sleep 1
	pid=$(systemctl show -p MainPID --value "$SERVICE")
	sleep 3
	systemctl is-active --quiet "$SERVICE" &&
		[[ -n $pid && $pid != 0 && $(systemctl show -p MainPID --value "$SERVICE") == "$pid" ]]
}

show_journal() {
	journalctl -u "$SERVICE" -n "${1:-8}" --no-pager -o cat 2>/dev/null | sed 's/^/    /' || true
}

reload_relay() {
	if systemctl is-active --quiet "$SERVICE"; then
		systemctl reload "$SERVICE"
		ok "relay перечитал список пользователей, остальные туннели не прерывались"
	fi
}

# ── what the player sees at the end ────────────────────────────────────────────────────────

# card FILE: the data to enter in the mod, from the name=/secret=/port= lines of the relay.
card() {
	local name secret port address friends
	name=$(sed -n 's/^name=//p' "$1")
	secret=$(sed -n 's/^secret=//p' "$1")
	port=$(sed -n 's/^port=//p' "$1")
	address=$(saved_address)
	address=${address:-"<IP сервера>"}
	friends=$address
	[[ $port != 25565 ]] && friends="$address:$port"
	local bar="$GREEN▌$R"
	printf '\n'
	printf '  %s %s%sДанные для мода%s\n' "$bar" "$GOLD" "$BOLD" "$R"
	printf '  %s %sВ игре: Esc → MCTunnel → вкладка «Сервер»%s\n' "$bar" "$GRAY" "$R"
	printf '  %s\n' "$bar"
	printf '  %s %s%s%s%s%s\n' "$bar" "$GRAY" "$(pad "Адрес relay" 15)" "$AQUA$BOLD" "$address" "$R"
	printf '  %s %s%s%s%s%s\n' "$bar" "$GRAY" "$(pad "Пользователь" 15)" "$AQUA$BOLD" "$name" "$R"
	printf '  %s %s%s%s%s%s\n' "$bar" "$GRAY" "$(pad "Секрет" 15)" "$AQUA$BOLD" "$secret" "$R"
	printf '  %s\n' "$bar"
	printf '  %s %sДрузья зайдут по адресу %s%s%s%s (Сетевая игра → Прямое подключение).%s\n' \
		"$bar" "$GRAY" "$WHITE$BOLD" "$friends" "$R" "$GRAY" "$R"
	printf '\n'
	warn "Секрет — как пароль: не публикуйте его. Показать снова: запустите установщик ещё раз."
	listeners
	if [[ $LISTEN_TCP != 25500 || ${LISTEN_TLS:-443} != 443 || ${LISTEN_KCP:-25500} != 25500 ]]; then
		info "Порты не стандартные: впишите их в моде на вкладке «Протокол» —"
		hint "TCP ${LISTEN_TCP:-выключен}, TLS ${LISTEN_TLS:-выключен}, KCP ${LISTEN_KCP:-выключен}."
	elif [[ -z $LISTEN_TLS || -z $LISTEN_KCP ]]; then
		info "Часть протоколов на сервере выключена: в моде на вкладке «Протокол» можно снять с них галочки."
	fi
}

provider_reminder() {
	ports_of
	info "Если у хостера есть свой файрвол (в панели управления VPS), откройте там"
	hint "$(ports_text)."
}

# ── actions ────────────────────────────────────────────────────────────────────────────────

wizard() {
	local name=$OPT_USER address=$OPT_ADDRESS detected
	local tcp=25500 tls=443 kcp=25500 pmin=25565 pmax=25664

	title "Установка relay"
	hint "Три вопроса — и сервер готов. Enter оставляет значение в скобках."

	printf '\n  %sШаг 1 из 3%s · %sимя пользователя%s\n' "$AQUA$BOLD" "$R" "$WHITE" "$R"
	hint "Его вписывают в мод вместе с секретом. Друзьям, которые заходят в ваш мир,"
	hint "ничего вписывать не нужно."
	while :; do
		[[ -n $name ]] || ask name "Имя (латиница, цифры, . _ -)" "steve"
		valid_name "$name" && break
		warn "«$name» не подходит: от 1 до 32 символов A-Z a-z 0-9 . _ -"
		interactive || exit 1
		name=""
	done

	printf '\n  %sШаг 2 из 3%s · %sадрес сервера%s\n' "$AQUA$BOLD" "$R" "$WHITE" "$R"
	if [[ -z $address ]]; then
		info "Узнаю внешний IP…"
		detected=$(detect_ip || true)
		if [[ -n $detected ]]; then ok "внешний IP: $detected"; else warn "Внешний IP узнать не удалось."; fi
		hint "Этот адрес вы впишете в мод, по нему же зайдут друзья. Можно указать домен."
		while :; do
			ask address "Адрес для мода" "$detected"
			valid_address "$address" && break
			warn "Нужен IP-адрес или домен (латиница, цифры, точки и дефисы)."
			interactive || exit 1
		done
	fi
	valid_address "$address" || die "Адрес «$address» не подходит."

	printf '\n  %sШаг 3 из 3%s · %sпорты и файрвол%s\n' "$AQUA$BOLD" "$R" "$WHITE" "$R"
	if [[ -n $OPT_PORTS ]]; then
		local range
		read -r tcp tls kcp range <<<"$OPT_PORTS"
		pmin=${range%-*} pmax=${range#*-}
	else
		if listening tcp | grep -qx 443; then
			warn "Порт 443 уже занят (веб-сервером?): TLS будет выключен, TCP и KCP работают."
			tls=0
		fi
		printf '    %s%s%s подключение хостов (из мода)\n' "$WHITE" "$(pad "TCP $tcp" 18)" "$R"
		((tls != 0)) && printf '    %s%s%s TLS: для сетей, где режут необычные порты\n' "$WHITE" "$(pad "TCP $tls" 18)" "$R"
		printf '    %s%s%s KCP: для Wi-Fi и мобильного интернета с потерями\n' "$WHITE" "$(pad "UDP $kcp" 18)" "$R"
		printf '    %s%s%s друзья заходят в миры (порт на пользователя)\n' "$WHITE" "$(pad "TCP $pmin–$pmax" 18)" "$R"
		if ! confirm "Оставить эти порты?" y; then
			while :; do ask tcp "TCP-порт для хостов" "$tcp"; valid_port "$tcp" && break; warn "Порт — число от 1 до 65535."; done
			while :; do ask tls "TLS-порт (0 — без TLS)" "$tls"; [[ $tls == 0 ]] || valid_port "$tls" && break; warn "Порт — число от 1 до 65535 или 0."; done
			while :; do ask kcp "UDP-порт для KCP (0 — без KCP)" "$tcp"; [[ $kcp == 0 ]] || valid_port "$kcp" && break; warn "Порт — число от 1 до 65535 или 0."; done
			while :; do
				ask pmin "Первый порт для друзей (по одному на пользователя, всего 100)" "$pmin"
				valid_port "$pmin" && ((pmin + 99 <= 65535)) && break
				warn "Нужен порт от 1 до 65436."
			done
			pmax=$((pmin + 99))
		fi
	fi
	local busy
	busy=$(listening tcp | awk -v a="$tcp" -v b="$tls" -v lo="$pmin" -v hi="$pmax" '$1 == a || $1 == b || ($1 >= lo && $1 <= hi)' | head -n 3 | tr '\n' ' ')
	[[ -z $busy ]] || die "Порты уже заняты другой программой: $busy" "Запустите установщик ещё раз и выберите другие порты."
	if ((kcp != 0)) && listening udp | grep -qx "$kcp"; then
		die "UDP-порт $kcp уже занят другой программой." "Запустите установщик ещё раз и выберите другой порт."
	fi
	local open=("$tcp/tcp" "$pmin-$pmax/tcp") firewall=0
	((tls == 0)) || open+=("$tls/tcp")
	((kcp == 0)) || open+=("$kcp/udp")
	printf '\n'
	if firewall_step "${open[@]}"; then firewall=1; fi

	printf '\n'
	confirm "Устанавливаю relay для «$name» на $address?" y || { info "Отменено, ничего не изменено."; exit 0; }

	title "Установка"
	stage_binary
	ensure_sysuser
	"$BIN.new" init -config "$CONF" -user "$name" -tcp "$tcp" -tls "$tls" -kcp "$kcp" -players "$pmin-$pmax" >"$WORK/user" ||
		die "Не удалось создать настройки $CONF."
	chgrp "$SYSUSER" "$CONF"
	chmod 0640 "$CONF"
	ok "настройки: $CONF"
	mv -f "$BIN.new" "$BIN"
	ok "relay: $BIN"
	save_address "$address"
	write_sysctl
	if ((firewall == 0)) || ! lockdown "${FIREWALL_KEEP[@]}"; then
		open_firewall
	fi
	write_unit
	mv -f "$UNIT.new" "$UNIT"
	systemctl daemon-reload
	systemctl enable --quiet "$SERVICE"
	systemctl restart "$SERVICE" || true
	if ! relay_up; then
		show_journal 12
		die "Relay не запустился." "Причина — в строках выше. Исправьте её и запустите установщик ещё раз."
	fi
	ok "служба $SERVICE работает и запустится сама после перезагрузки"

	toast "Свой relay-сервер"
	card "$WORK/user"
	provider_reminder
	printf '\n  %sГотово! Откройте мир для сети — адрес для друзей появится в чате.%s\n\n' "$GREEN$BOLD" "$R"
}

# update: the new binary (and unit, sysctl file) with the installed configuration, or with
# $NEW_CONFIG. Also the first installation from deploy.ps1 with -Config.
action_update() {
	local before after cfg backup="" had_unit=0 bin_changed=0 unit_changed=0
	cfg=${NEW_CONFIG:-$CONF}
	title "$(installed && printf 'Обновление relay' || printf 'Установка relay')"
	before=$([[ -x $BIN ]] && relay_version "$BIN" || true)
	stage_binary
	after=$(relay_version "$BIN.new")
	ensure_sysuser
	if ! "$BIN.new" -config "$cfg" -check >"$WORK/check" 2>&1; then
		sed 's/^/    /' "$WORK/check" >&2
		die "Новый relay не принимает настройки $cfg." "Ничего не изменено."
	fi

	if ! cmp -s "$BIN.new" "$BIN"; then
		[[ -f $BIN ]] && cp -p "$BIN" "$BIN.prev"
		mv -f "$BIN.new" "$BIN"
		bin_changed=1
	fi
	if [[ -n $NEW_CONFIG ]]; then
		if [[ -f $CONF ]] && ! cmp -s "$NEW_CONFIG" "$CONF"; then
			backup="$CONF.bak.$(date +%Y%m%d-%H%M%S)"
			cp -p "$CONF" "$backup"
			info "прежние настройки сохранены: $backup"
		fi
		install -m 0640 -o root -g "$SYSUSER" "$NEW_CONFIG" "$CONF.new"
		mv -f "$CONF.new" "$CONF"
		ok "настройки: $CONF"
	elif [[ $(stat -c %U:%G:%a "$CONF") != "root:$SYSUSER:640" ]]; then
		# Written by hand (nano, cp without -p): the relay runs as $SYSUSER and must read it,
		# nobody else should.
		chown "root:$SYSUSER" "$CONF"
		chmod 0640 "$CONF"
		info "$CONF: владелец root:$SYSUSER, права 0640"
	fi
	write_sysctl
	open_firewall
	write_unit
	if [[ -f $UNIT ]]; then
		had_unit=1
		cp -p "$UNIT" "$WORK/unit.prev"
	fi
	if ! cmp -s "$UNIT.new" "$UNIT"; then
		mv -f "$UNIT.new" "$UNIT"
		unit_changed=1
		systemctl daemon-reload
	fi
	systemctl enable --quiet "$SERVICE"

	if ((bin_changed == 0 && unit_changed == 0)) && [[ -z $backup && -z $NEW_CONFIG ]] && systemctl is-active --quiet "$SERVICE"; then
		ok "уже установлена версия $after, relay работает"
		return 0
	fi
	info "Перезапускаю relay: туннели переподключатся сами за несколько секунд."
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	systemctl restart "$SERVICE" || true
	if relay_up; then
		if [[ -n $before && $before != "$after" ]]; then
			ok "relay обновлён: $before → $after, работает"
		else
			ok "relay $after работает"
		fi
		return 0
	fi

	show_journal 12
	warn "Relay не остался запущенным: возвращаю прежнюю версию."
	local restored=0
	if ((bin_changed == 1)) && [[ -f $BIN.prev ]]; then cp -p "$BIN.prev" "$BIN" && restored=1; fi
	if [[ -n $backup ]]; then install -m 0640 -o root -g "$SYSUSER" "$backup" "$CONF" && restored=1; fi
	if ((had_unit == 1 && unit_changed == 1)); then
		install -m 0644 "$WORK/unit.prev" "$UNIT" && systemctl daemon-reload && restored=1
	fi
	if ((restored == 1)); then
		systemctl reset-failed "$SERVICE" 2>/dev/null || true
		systemctl restart "$SERVICE" || true
		if relay_up; then
			die "Обновление не удалось, прежняя версия снова работает." "Причина — в журнале выше."
		fi
	fi
	die "Relay не запускается." "Причина — в журнале выше: sudo journalctl -u $SERVICE -n 50"
}

# pick_user QUESTION: PICKED = a user chosen by number or name (the only one, if one).
pick_user() {
	local lines=() line n p s i answer
	mapfile -t lines < <("$BIN" user list -config "$CONF")
	PICKED=""
	if ((${#lines[@]} == 1)); then
		PICKED=${lines[0]%%$'\t'*}
		return 0
	fi
	i=1
	for line in "${lines[@]}"; do
		IFS=$'\t' read -r n p s <<<"$line"
		printf '    %s%d%s  %s  %s(порт %s%s)%s\n' "$AQUA$BOLD" "$i" "$R" "$n" "$DGRAY" "$p" \
			"$([[ $s == off ]] && printf ', выключен' || true)" "$R"
		i=$((i + 1))
	done
	interactive || return 1
	ask answer "$1 (номер или имя, Enter — отмена)" ""
	[[ -n $answer ]] || return 1
	if [[ $answer =~ ^[0-9]+$ ]] && ((answer >= 1 && answer <= ${#lines[@]})); then
		PICKED=${lines[answer - 1]%%$'\t'*}
		return 0
	fi
	for line in "${lines[@]}"; do
		[[ ${line%%$'\t'*} == "$answer" ]] && PICKED=$answer && return 0
	done
	warn "Нет такого пользователя: $answer"
	return 1
}

action_show() {
	local name=${1:-} address detected
	require_manageable || return 1
	if [[ -z $(saved_address) ]]; then
		detected=$(detect_ip || true)
		ask address "Адрес сервера для мода" "$detected"
		if valid_address "$address"; then save_address "$address"; fi
	fi
	if [[ -z $name ]]; then
		pick_user "Чьи данные показать?" || return 0
		name=$PICKED
	fi
	"$BIN" user show -config "$CONF" "$name" >"$WORK/user" 2>"$WORK/err" || { warn "$(cat "$WORK/err")"; return 1; }
	card "$WORK/user"
}

action_add() {
	local name=${1:-} err
	title "Новый пользователь"
	hint "Каждый, кто открывает через этот relay свой мир, — отдельный пользователь со своим"
	hint "секретом и своим адресом для друзей."
	require_manageable || return 1
	while :; do
		if [[ -z $name ]]; then
			interactive || die "Укажите имя: add ИМЯ"
			ask name "Имя (латиница, цифры, . _ -; Enter — отмена)" ""
			[[ -n $name ]] || return 0
		fi
		if ! valid_name "$name"; then
			warn "«$name» не подходит: от 1 до 32 символов A-Z a-z 0-9 . _ -"
		elif "$BIN" user show -config "$CONF" "$name" >/dev/null 2>&1; then
			warn "Пользователь $name уже есть."
		else
			break
		fi
		interactive || exit 1
		name=""
	done
	if ! "$BIN" user add -config "$CONF" "$name" >"$WORK/user" 2>"$WORK/err"; then
		err=$(cat "$WORK/err")
		warn "Не получилось: $err"
		return 1
	fi
	ok "пользователь $name добавлен"
	reload_relay
	card "$WORK/user"
}

action_remove() {
	local name=${1:-} answer count
	title "Удаление пользователя"
	require_manageable || return 1
	count=$("$BIN" user list -config "$CONF" | wc -l)
	if ((count <= 1)); then
		warn "Это единственный пользователь: relay без пользователей не работает."
		hint "Сначала добавьте другого или удалите MCTunnel целиком."
		return 1
	fi
	if [[ -z $name ]]; then
		pick_user "Кого удалить?" || return 0
		name=$PICKED
	fi
	if ((OPT_YES == 0)); then
		interactive || die "Для удаления без вопросов: remove ИМЯ --yes"
		warn "Туннель $name закроется, его друзья отключатся, секрет перестанет работать."
		ask answer "Чтобы подтвердить, введите имя ещё раз" ""
		[[ $answer == "$name" ]] || { info "Отменено."; return 0; }
	fi
	if ! "$BIN" user remove -config "$CONF" "$name" 2>"$WORK/err"; then
		warn "Не получилось: $(cat "$WORK/err")"
		return 1
	fi
	ok "пользователь $name удалён"
	reload_relay
}

action_list() {
	local line n p s
	title "Пользователи"
	require_manageable || return 1
	while IFS=$'\t' read -r n p s; do
		printf '    %s  %sпорт %s%s%s\n' "$(pad "$n" 20)" "$DGRAY" "$p" "$([[ $s == off ]] && printf ', выключен' || true)" "$R"
	done < <("$BIN" user list -config "$CONF")
}

action_firewall() {
	title "Файрвол"
	ports_of
	if ! firewall_step "${HOST_PORTS[@]}" "$PLAYER_RANGE/tcp"; then
		info "Файрвол не трогаю."
		return 0
	fi
	lockdown "${FIREWALL_KEEP[@]}"
}

action_restart() {
	title "Перезапуск relay"
	info "Туннели переподключатся сами за несколько секунд."
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	systemctl restart "$SERVICE" || true
	if relay_up; then ok "relay работает"; else show_journal 12; warn "Relay не запустился: причина — в строках выше."; fi
}

action_log() {
	title "Журнал relay"
	show_journal 25
	hint "Весь журнал: sudo journalctl -u $SERVICE -f"
}

action_uninstall() {
	local answer
	title "Удаление MCTunnel"
	warn "Relay остановится, друзья отключатся, настройки с секретами будут удалены."
	if ((OPT_YES == 0)); then
		interactive || die "Для удаления без вопросов: uninstall --yes"
		ask answer "Чтобы подтвердить, введите: удалить" ""
		case "$answer" in удалить | Удалить | УДАЛИТЬ | delete) ;; *) info "Отменено."; return 1 ;; esac
	fi
	[[ -x $BIN ]] && close_firewall
	systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
	rm -f "$UNIT"
	systemctl daemon-reload
	systemctl reset-failed "$SERVICE" >/dev/null 2>&1 || true
	ok "служба $SERVICE остановлена и удалена"
	rm -f "$BIN" "$BIN.prev" "$BIN.new"
	rm -rf "$ETC"
	ok "удалены $BIN и $ETC"
	rm -f "$SYSCTL" "$MODLOAD"
	userdel "$SYSUSER" >/dev/null 2>&1 || true
	groupdel "$SYSUSER" >/dev/null 2>&1 || true
	ok "удалён системный пользователь $SYSUSER"
	hint "BBR остаётся включённым до перезагрузки сервера."
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		hint "Файрвол остаётся включённым: порты relay закрыты, SSH открыт (выключить: sudo ufw disable)."
	fi
	printf '\n  %sMCTunnel удалён. Спасибо, что играли!%s\n\n' "$GREEN$BOLD" "$R"
	return 0
}

action_auto() {
	if [[ -n $HERE && -f $HERE/relay.json ]]; then
		NEW_CONFIG=$HERE/relay.json
		action_update
	elif installed; then
		action_update
	else
		wizard
	fi
}

status_line() {
	local version state users address dot
	version=$([[ -x $BIN ]] && relay_version "$BIN" || true)
	if systemctl is-active --quiet "$SERVICE"; then
		dot="$GREEN●$R" state="работает"
	else
		dot="$RED●$R" state="остановлен"
	fi
	users="?"
	if manageable; then users=$("$BIN" user list -config "$CONF" 2>/dev/null | wc -l); fi
	address=$(saved_address)
	printf '\n  %s %srelay %s%s · %s · пользователей: %s%s\n' "$dot" "$WHITE$BOLD" "${version:-?}" "$R" "$state" "$users" \
		"$([[ -n $address ]] && printf ' · %s' "$address" || true)"
}

menu() {
	local choice target=$RELEASE
	[[ $target == dev ]] && target="последней версии"
	while :; do
		status_line
		printf '\n'
		printf '    %s1%s  Обновить relay до %s\n' "$AQUA$BOLD" "$R" "$target"
		printf '    %s2%s  Показать данные для мода\n' "$AQUA$BOLD" "$R"
		printf '    %s3%s  Добавить пользователя\n' "$AQUA$BOLD" "$R"
		printf '    %s4%s  Удалить пользователя\n' "$AQUA$BOLD" "$R"
		printf '    %s5%s  Перезапустить relay\n' "$AQUA$BOLD" "$R"
		printf '    %s6%s  Журнал relay\n' "$AQUA$BOLD" "$R"
		printf '    %s7%s  Файрвол: закрыть всё, кроме relay и SSH\n' "$AQUA$BOLD" "$R"
		printf '    %s8%s  Удалить MCTunnel с сервера\n' "$AQUA$BOLD" "$R"
		printf '    %s0%s  Выход\n' "$AQUA$BOLD" "$R"
		interactive || return 0
		ask choice "Выберите" "0"
		case "$choice" in
			1) action_update || true ;;
			2) action_show "" || true ;;
			3) action_add "" || true ;;
			4) action_remove "" || true ;;
			5) action_restart || true ;;
			6) action_log || true ;;
			7) action_firewall || true ;;
			8) action_uninstall && return 0 ;;
			0 | q | й | exit | выход) printf '\n'; return 0 ;;
			*) warn "Нет такого пункта: $choice" ;;
		esac
	done
}

usage() {
	cat <<EOF
MCTunnel relay installer
  install.sh                        first installation, or the menu when installed
  install.sh install [--user NAME] [--address ADDR] [--ports "TCP TLS KCP MIN-MAX"]
             [--keep "80 443/tcp"] [--no-firewall]
  install.sh update | list | add NAME | show NAME | remove NAME [--yes] | uninstall [--yes]
  install.sh firewall [--keep "80 443/tcp"]
  install.sh auto                   used by deploy.ps1
EOF
}

parse_args() {
	while (($# > 0)); do
		case "$1" in
			install | update | list | firewall | uninstall | auto) ACTION=$1 ;;
			add | show | remove)
				ACTION=$1
				if (($# > 1)) && [[ $2 != --* ]]; then ACTION_ARG=$2; shift; fi
				;;
			--user) OPT_USER=${2:-}; shift ;;
			--address) OPT_ADDRESS=${2:-}; shift ;;
			--ports) OPT_PORTS=${2:-}; shift ;;
			--keep) OPT_KEEP=${2:-}; shift ;;
			--no-firewall) OPT_NO_FIREWALL=1 ;;
			--yes | -y) OPT_YES=1 ;;
			-h | --help | help) usage; exit 0 ;;
			*) usage >&2; die "Непонятный аргумент: $1" ;;
		esac
		shift
	done
}

main() {
	# Cyrillic answers are matched and measured as characters.
	if locale -a 2>/dev/null | grep -qix 'c\.utf-\?8'; then export LC_ALL=C.UTF-8; fi
	setup_output
	parse_args "$@"
	setup_input
	check_system
	if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then
		HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
	fi
	WORK=$(mktemp -d)
	trap cleanup EXIT
	trap 'on_error "$LINENO" "$BASH_COMMAND"' ERR
	banner
	case "$ACTION" in
		"") if installed; then menu; else wizard; fi ;;
		install)
			installed && die "Relay уже установлен." "Запустите установщик без аргументов: откроется меню."
			wizard
			;;
		update) require_installed; action_update || exit 1 ;;
		list) require_installed; action_list || exit 1 ;;
		add) require_installed; action_add "$ACTION_ARG" || exit 1 ;;
		show) require_installed; action_show "$ACTION_ARG" || exit 1 ;;
		remove) require_installed; action_remove "$ACTION_ARG" || exit 1 ;;
		firewall) require_installed; action_firewall || exit 1 ;;
		uninstall) require_installed; action_uninstall || exit 1 ;;
		auto) action_auto || exit 1 ;;
	esac
}

# Everything above only defines functions: a download cut short runs nothing.
main "$@"
