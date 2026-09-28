#!/usr/bin/env bash
# MCTunnel relay installer for a Linux VPS with systemd (Ubuntu, Debian and the like).
#
# One command on the server:
#   curl -fsSL https://github.com/Donkoev/MCTunnel/releases/latest/download/install.sh | sudo bash
#
# It first asks for the language (Русский or English). The first run then asks three questions
# (a user name, the server's address, the ports), downloads the relay of this release, checks its
# SHA-256, installs it as the systemd service mctunnel-relay, turns on a firewall that leaves
# only the relay and SSH open (ufw; what else listens is shown first, and can be kept) and prints
# what to enter in the mod. Every later run opens a menu: update the relay, show a user's data
# for the mod, add or remove users, restart, the log, the firewall, uninstall.
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
#   --lang ru|en            the language, without the question (or MCTUNNEL_LANG)
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

UI="ru"       # the language of every text: ru or en (choose_language)
HERE=""       # the script's directory when it runs from a file; empty after a pipe
WORK=""       # temporary directory, removed on exit
TTY=""        # where answers are read from; empty: no terminal, defaults are taken
ARCH=""
ACTION=""
ACTION_ARG=""
OPT_LANG=""
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

# L RUSSIAN ENGLISH: the text in the chosen language.
L() {
	if [[ $UI == en ]]; then printf '%s' "$2"; else printf '%s' "$1"; fi
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
SPLASHES_RU=(
	"Без белого IP!" "Работает за CGNAT!" "Друзьям мод не нужен!" "Теперь с KCP!"
	"Копаем туннель…" "Свой сервер — свои правила!" "Секрет не покидает ПК!"
)
SPLASHES_EN=(
	"No public IP needed!" "Works behind CGNAT!" "Friends need no mod!" "Now with KCP!"
	"Digging a tunnel…" "Your server, your rules!" "The secret never leaves your PC!"
)

banner() {
	local version=$RELEASE splash
	[[ $version == dev ]] && version=$(L "из исходников" "from source")
	if ((COLOR == 0)); then
		printf '\n  %s\n' "$(L "MCTunnel · relay-сервер для вашего мира Minecraft · $version" \
			"MCTunnel · relay server for your Minecraft world · $version")"
		return
	fi
	if [[ $UI == en ]]; then
		splash=${SPLASHES_EN[RANDOM % ${#SPLASHES_EN[@]}]}
	else
		splash=${SPLASHES_RU[RANDOM % ${#SPLASHES_RU[@]}]}
	fi
	local -A pixel=([G]=107 [g]=71 [k]=64 [D]=94 [d]=58 [L]=137)
	local text=(
		""
		"${WHITE}${BOLD}MCTunnel${R}  ${DGRAY}$(L "установщик relay" "relay installer") · ${version}${R}"
		"${GRAY}$(L "Ваш мир Minecraft — друзьям, через свой VPS." "Your Minecraft world for your friends, through your own VPS.")${R}"
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
	box "$DGRAY" "${YELLOW}★ $(L "Достижение получено!" "Advancement Made!")${R}" "  ${WHITE}$1${R}"
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

# choose_language: UI from --lang or MCTUNNEL_LANG; else asked first thing (the language saved by
# an earlier run is the default); without a terminal the saved one, else Russian.
choose_language() {
	local saved answer default=1
	case "${OPT_LANG:-${MCTUNNEL_LANG:-}}" in
		ru* | RU*) UI=ru; return ;;
		en* | EN*) UI=en; return ;;
	esac
	saved=$(state_get LANGUAGE)
	[[ $saved == en ]] && default=2
	if ! interactive; then
		UI=${saved:-ru}
		[[ $UI == en ]] || UI=ru
		return
	fi
	printf '\n  %sMCTunnel%s\n\n' "$WHITE$BOLD" "$R"
	printf '    %s1%s  Русский\n' "$AQUA$BOLD" "$R"
	printf '    %s2%s  English\n' "$AQUA$BOLD" "$R"
	while :; do
		printf '  %s?%s Язык / Language %s[%s]%s: ' "$AQUA$BOLD" "$R" "$DGRAY" "$default" "$R"
		IFS= read -r answer <"$TTY" || true
		answer=${answer%$'\r'}
		answer=${answer// /}
		case "${answer:-$default}" in
			1 | ru | RU | Ru | р | Р | русский | Русский) UI=ru; return ;;
			2 | en | EN | En | english | English) UI=en; return ;;
		esac
	done
}

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
# y, yes (and l, the д key on a Latin layout) mean yes; н, нет, n, no mean no.
confirm() {
	local answer default=${2:-y} choices
	if ! interactive; then
		[[ $default == y ]]
		return
	fi
	if [[ $default == y ]]; then choices=$(L "Д/н" "Y/n"); else choices=$(L "д/Н" "y/N"); fi
	while :; do
		ask answer "$1 ${DGRAY}(${choices})${R}" ""
		case "$answer" in
			"") [[ $default == y ]]; return ;;
			д | Д | да | Да | ДА | l | L | y | Y | yes | Yes | YES) return 0 ;;
			н | Н | нет | Нет | НЕТ | n | N | no | No | NO) return 1 ;;
			*) warn "$(L "Ответьте «д» (да) или «н» (нет)." "Answer y (yes) or n (no).")" ;;
		esac
	done
}

valid_name() { [[ $1 =~ ^[A-Za-z0-9._-]{1,32}$ ]]; }
valid_address() { [[ $1 =~ ^[A-Za-z0-9.:-]{1,253}$ ]]; }
valid_port() { [[ $1 =~ ^[0-9]{1,5}$ ]] && (($1 >= 1 && $1 <= 65535)); }

# ── system ──────────────────────────────────────────────────────────────────────────────────

check_system() {
	[[ $(uname -s) == Linux ]] || die "$(L "Нужен сервер на Linux." "This needs a Linux server.")"
	if ((EUID != 0)); then
		die "$(L "Нужны права root." "This needs root.")" \
			"$(L "Запустите так:" "Run it like this:") curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sudo bash"
	fi
	if ! command -v systemctl >/dev/null 2>&1 || [[ ! -d /run/systemd/system ]]; then
		die "$(L "Нужен systemd (Ubuntu, Debian и похожие системы)." "This needs systemd (Ubuntu, Debian and the like).")"
	fi
	case "$(uname -m)" in
		x86_64 | amd64) ARCH=amd64 ;;
		aarch64 | arm64) ARCH=arm64 ;;
		*) die "$(L "Процессор $(uname -m) не поддерживается: нужен x86_64 или arm64." \
			"The $(uname -m) processor is not supported: x86_64 or arm64 is needed.")" ;;
	esac
}

cleanup() {
	rm -f "$BIN.new" "$UNIT.new" "$CONF.new"
	[[ -n $WORK ]] && rm -rf "$WORK"
	return 0
}

on_error() {
	printf '\n  %s✘ %s%s\n' "$RED$BOLD" "$(L "Что-то пошло не так (строка $1: $2)." "Something went wrong (line $1: $2).")" "$R" >&2
	printf '    %s\n' "$(L "Запустите установщик ещё раз; если не поможет, пришлите этот вывод:" \
		"Run the installer again; if that does not help, send this output:")" >&2
	printf '    https://github.com/%s/issues\n\n' "$REPO" >&2
}

installed() { [[ -f $CONF ]]; }

require_installed() {
	installed || die "$(L "MCTunnel relay здесь не установлен ($CONF нет)." "The MCTunnel relay is not installed here (no $CONF).")" \
		"$(L "Запустите установщик без аргументов." "Run the installer without arguments.")"
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
	warn "$(L "Установленный relay слишком старый для этого: сначала обновите его (пункт 1 меню)." \
		"The installed relay is too old for this: update it first (menu item 1).")"
	return 1
}

# fetch URL FILE
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 20 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 20 -t 3 -O "$2" "$1"
	else
		die "$(L "Нужен curl или wget." "This needs curl or wget.")" "$(L "Установите:" "Install it:") apt install curl"
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
		ok "$(L "relay $(relay_version "$BIN.new") ($ARCH) из $HERE" "relay $(relay_version "$BIN.new") ($ARCH) from $HERE")"
		return
	fi
	info "$(L "Скачиваю relay с GitHub ($REPO)…" "Downloading the relay from GitHub ($REPO)…")"
	fetch "$(release_url "$asset")" "$WORK/$asset" ||
		die "$(L "Не удалось скачать relay." "Could not download the relay.")" "$(release_url "$asset")" \
			"$(L "Проверьте, что сервер открывает github.com." "Check that the server can reach github.com.")"
	fetch "$(release_url SHA256SUMS.txt)" "$WORK/SHA256SUMS.txt" ||
		die "$(L "Не удалось скачать контрольные суммы." "Could not download the checksums.")" "$(release_url SHA256SUMS.txt)"
	# The list may come with Windows line endings; names may carry a folder ("relay/...").
	want=$(tr -d '\r' <"$WORK/SHA256SUMS.txt" | awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n); sub(/.*\//, "", n); if (n == a) print $1 }' | head -n 1)
	got=$(sha256sum "$WORK/$asset" | awk '{print $1}')
	if [[ -z $want || $want != "$got" ]]; then
		die "$(L "Контрольная сумма relay не совпала: файл повреждён или подменён." \
			"The relay's checksum does not match: the file is damaged or tampered with.")" \
			"$(L "Ничего не установлено." "Nothing was installed.")"
	fi
	install -m 0755 "$WORK/$asset" "$BIN.new"
	ok "$(L "скачан relay $(relay_version "$BIN.new") ($ARCH), SHA-256 совпал" \
		"downloaded relay $(relay_version "$BIN.new") ($ARCH), SHA-256 matches")"
}

ensure_sysuser() {
	if ! id "$SYSUSER" >/dev/null 2>&1; then
		useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$SYSUSER"
		ok "$(L "системный пользователь $SYSUSER (relay работает без прав root)" "system user $SYSUSER (the relay runs without root)")"
	fi
	install -d -m 0750 -o root -g "$SYSUSER" "$ETC"
}

# state_get KEY / state_set KEY VALUE: what the installer remembers in $STATE (the server's
# address for the mod, the language).
state_get() {
	[[ -r $STATE ]] && sed -n "s/^$1=//p" "$STATE" 2>/dev/null | head -n 1
	return 0
}

state_set() {
	local tmp
	[[ -d $ETC ]] || return 0
	tmp=$(mktemp)
	{
		[[ -f $STATE ]] && grep -v "^$1=" "$STATE"
		printf '%s=%s\n' "$1" "$2"
	} >"$tmp" || true
	install -m 0644 "$tmp" "$STATE"
	rm -f "$tmp"
}

saved_address() { state_get ADDRESS; }
save_address() { state_set ADDRESS "$1"; }

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
		ok "$(L "BBR включён, буферы UDP увеличены" "BBR on, UDP buffers raised")"
	else
		info "$(L "BBR на этом ядре недоступен: relay работает и без него" "BBR is not available on this kernel: the relay works without it")"
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
		printf 'TCP %s %s UDP %s' "$tcp" "$(L "и" "and")" "$(printf '%s, ' "${UDP_PORTS[@]}" | sed 's/, $//')"
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
		ok "$(L "ufw: открыты" "ufw: opened") $(ports_text)"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		for p in "${HOST_PORTS[@]}"; do firewall-cmd -q --permanent --add-port="$p"; done
		firewall-cmd -q --permanent --add-port="$PLAYER_RANGE/tcp"
		firewall-cmd -q --reload
		ok "$(L "firewalld: открыты" "firewalld: opened") $(ports_text)"
	else
		info "$(L "файрвола на сервере нет (ufw и firewalld выключены)" "no firewall on the server (ufw and firewalld are off)")"
	fi
}

close_firewall() {
	local p
	ports_of 2>/dev/null || return 0
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		for p in "${HOST_PORTS[@]}"; do ufw delete allow "$p" >/dev/null 2>&1 || true; done
		ufw delete allow "${PLAYER_RANGE/-/:}/tcp" >/dev/null 2>&1 || true
		ok "$(L "ufw: порты закрыты" "ufw: ports closed")"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		for p in "${HOST_PORTS[@]}"; do firewall-cmd -q --permanent --remove-port="$p" || true; done
		firewall-cmd -q --permanent --remove-port="$PLAYER_RANGE/tcp" || true
		firewall-cmd -q --reload || true
		ok "$(L "firewalld: порты закрыты" "firewalld: ports closed")"
	fi
}

# ── firewall: from the outside only the relay, SSH and what the system itself needs ─────────

# ssh_ports: the TCP ports SSH is reachable on, one per line: the listening sockets of sshd (or
# dropbear), its systemd socket, its configuration, and the port of every open SSH session.
# Loopback sockets do not count: sshd listens there for X11 forwarding (6010, ...) and forwarded
# ports, which are no way in from the outside. Empty when there is no SSH: the firewall is then
# left alone, it could lock the owner out.
ssh_ports() {
	{
		ss -H -ltnp 2>/dev/null |
			awk '/"(sshd|dropbear)"/ && $4 !~ /^(127\.|\[::1\]|\[::ffff:127\.)/ {n = split($4, a, ":"); print a[n]}'
		systemctl show -p Listen ssh.socket sshd.socket 2>/dev/null | sed -n 's/^Listen=.*:\([0-9][0-9]*\) (Stream)$/\1/p'
		if command -v sshd >/dev/null 2>&1; then sshd -T 2>/dev/null | awk '$1 == "port" {print $2}'; fi
		ss -H -tnp state established 2>/dev/null |
			awk '/"(sshd|dropbear)"/ && $3 !~ /^(127\.|\[::1\]|\[::ffff:127\.)/ {n = split($3, a, ":"); print a[n]}'
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
	[[ -n $ssh ]] || ssh=$(L "не найден" "not found")
	mapfile -t others < <(other_listeners "$@" $(ssh_ports | sed 's|$|/tcp|'))
	hint "$(L "Файрвол: снаружи останутся открыты только порты relay и SSH ($ssh)," \
		"Firewall: from the outside only the relay's ports and SSH ($ssh) stay open,")"
	hint "$(L "все остальные закроются. Исходящие соединения, ping и системные службы работают как раньше." \
		"everything else is closed. Outgoing connections, ping and system services keep working.")"
	if ((${#others[@]} > 0)); then
		warn "$(L "Сейчас снаружи доступны и закроются:" "Reachable from the outside now, and about to be closed:")"
		for line in "${others[@]}"; do
			read -r proto port proc <<<"$line"
			hint "  ${proto^^} $port ($proc)"
		done
		if [[ -n $OPT_KEEP ]]; then
			for p in $OPT_KEEP; do
				valid_keep "$p" || die "$(L "--keep: «$p» — не порт (80, 80/tcp или 8000:8010/tcp)." \
					"--keep: \"$p\" is not a port (80, 80/tcp or 8000:8010/tcp).")"
				FIREWALL_KEEP+=("$p")
			done
		elif interactive; then
			while :; do
				ask answer "$(L "Оставить какие-то открытыми? Номера через пробел, например 80 443/tcp (Enter — нет)" \
					"Keep any of them open? Numbers separated by spaces, e.g. 80 443/tcp (Enter: none)")" ""
				bad="" FIREWALL_KEEP=()
				for p in $answer; do
					if valid_keep "$p"; then FIREWALL_KEEP+=("$p"); else bad+=" $p"; fi
				done
				[[ -n $bad ]] || break
				warn "$(L "Не понял:$bad. Порт — число (можно с /tcp или /udp), диапазон — 8000:8010/tcp." \
					"Not understood:$bad. A port is a number (optionally with /tcp or /udp), a range is 8000:8010/tcp.")"
			done
		fi
	fi
	confirm "$(L "Включить файрвол?" "Turn the firewall on?")" y
}

# lockdown KEEP...: turns the firewall (ufw) on so that only the relay, SSH and KEEP are open to
# the outside. Outgoing connections and the answers to them, loopback, ping and DHCP keep working
# (ufw's own rules). The old ufw rules are backed up by `ufw reset`.
lockdown() {
	local ssh=() p
	mapfile -t ssh < <(ssh_ports)
	if ((${#ssh[@]} == 0)); then
		warn "$(L "Не нашёл, на каком порту работает SSH: файрвол не трогаю, чтобы не потерять доступ к серверу." \
			"Could not find the port SSH runs on: leaving the firewall alone so as not to lose access to the server.")"
		return 1
	fi
	if ! command -v ufw >/dev/null 2>&1; then
		if ! command -v apt-get >/dev/null 2>&1; then
			warn "$(L "Здесь нет ufw: закройте лишние порты файрволом своей системы." \
				"There is no ufw here: close the other ports with your system's firewall.")"
			return 1
		fi
		info "$(L "Ставлю файрвол ufw…" "Installing the ufw firewall…")"
		if ! DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ufw >/dev/null 2>&1; then
			warn "$(L "Не удалось поставить ufw: закройте лишние порты сами." "Could not install ufw: close the other ports yourself.")"
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
	if (($# == 0)); then
		ok "$(L "файрвол включён: снаружи открыты только SSH (${ssh[*]}) и relay" \
			"firewall on: from the outside only SSH (${ssh[*]}) and the relay are open")"
	else
		ok "$(L "файрвол включён: снаружи открыты только SSH (${ssh[*]}), relay и $*" \
			"firewall on: from the outside only SSH (${ssh[*]}), the relay and $* are open")"
	fi
	hint "$(L "Остальные порты закрыты. Прежние правила ufw сохранены в /etc/ufw/*.rules.<время>." \
		"All other ports are closed. The previous ufw rules are saved in /etc/ufw/*.rules.<time>.")"
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
		ok "$(L "relay перечитал список пользователей, остальные туннели не прерывались" \
			"the relay reloaded its users; other tunnels were not interrupted")"
	fi
}

# ── what the player sees at the end ────────────────────────────────────────────────────────

# card FILE: the data to enter in the mod, from the name=/secret=/port= lines of the relay.
card() {
	local name secret port address friends labels=15
	name=$(sed -n 's/^name=//p' "$1")
	secret=$(sed -n 's/^secret=//p' "$1")
	port=$(sed -n 's/^port=//p' "$1")
	address=$(saved_address)
	address=${address:-$(L "<IP сервера>" "<server IP>")}
	friends=$address
	[[ $port != 25565 ]] && friends="$address:$port"
	local bar="$GREEN▌$R"
	printf '\n'
	printf '  %s %s%s%s%s\n' "$bar" "$GOLD" "$BOLD" "$(L "Данные для мода" "Data for the mod")" "$R"
	printf '  %s %s%s%s\n' "$bar" "$GRAY" "$(L "В игре: Esc → MCTunnel → вкладка «Сервер»" "In the game: Esc → MCTunnel → the Relay tab")" "$R"
	printf '  %s\n' "$bar"
	printf '  %s %s%s%s%s%s\n' "$bar" "$GRAY" "$(pad "$(L "Адрес relay" "Relay address")" $labels)" "$AQUA$BOLD" "$address" "$R"
	printf '  %s %s%s%s%s%s\n' "$bar" "$GRAY" "$(pad "$(L "Пользователь" "User")" $labels)" "$AQUA$BOLD" "$name" "$R"
	printf '  %s %s%s%s%s%s\n' "$bar" "$GRAY" "$(pad "$(L "Секрет" "Secret")" $labels)" "$AQUA$BOLD" "$secret" "$R"
	printf '  %s\n' "$bar"
	printf '  %s %s%s %s%s%s%s %s%s\n' "$bar" "$GRAY" "$(L "Друзья зайдут по адресу" "Friends join at")" "$WHITE$BOLD" "$friends" "$R" "$GRAY" \
		"$(L "(Сетевая игра → Прямое подключение)." "(Multiplayer → Direct Connection).")" "$R"
	printf '\n'
	warn "$(L "Секрет — как пароль: не публикуйте его. Показать снова: запустите установщик ещё раз." \
		"The secret is like a password: do not share it. To see it again, run the installer again.")"
	listeners
	if [[ $LISTEN_TCP != 25500 || ${LISTEN_TLS:-443} != 443 || ${LISTEN_KCP:-25500} != 25500 ]]; then
		local off
		off=$(L "выключен" "off")
		info "$(L "Порты не стандартные: впишите их в моде на вкладке «Протокол» —" "Non-standard ports: enter them in the mod on the Protocol tab:")"
		hint "TCP ${LISTEN_TCP:-$off}, TLS ${LISTEN_TLS:-$off}, KCP ${LISTEN_KCP:-$off}."
	elif [[ -z $LISTEN_TLS || -z $LISTEN_KCP ]]; then
		info "$(L "Часть протоколов на сервере выключена: в моде на вкладке «Протокол» можно снять с них галочки." \
			"Some protocols are off on the server: you can uncheck them in the mod on the Protocol tab.")"
	fi
}

provider_reminder() {
	ports_of
	info "$(L "Если у хостера есть свой файрвол (в панели управления VPS), откройте там" \
		"If your hosting provider has its own firewall (in the VPS control panel), open these there:")"
	hint "$(ports_text)."
}

# ── actions ────────────────────────────────────────────────────────────────────────────────

# step N TITLE: the header of a wizard step.
step() {
	printf '\n  %s%s%s · %s%s%s\n' "$AQUA$BOLD" "$(L "Шаг $1 из 3" "Step $1 of 3")" "$R" "$WHITE" "$2" "$R"
}

wizard() {
	local name=$OPT_USER address=$OPT_ADDRESS detected
	local tcp=25500 tls=443 kcp=25500 pmin=25565 pmax=25664

	title "$(L "Установка relay" "Relay installation")"
	hint "$(L "Три вопроса — и сервер готов. Enter оставляет значение в скобках." \
		"Three questions and the server is ready. Enter keeps the value in brackets.")"

	step 1 "$(L "имя пользователя" "user name")"
	hint "$(L "Его вписывают в мод вместе с секретом. Друзьям, которые заходят в ваш мир," \
		"You enter it in the mod together with the secret. Friends who join your world")"
	hint "$(L "ничего вписывать не нужно." "do not need to enter anything.")"
	while :; do
		[[ -n $name ]] || ask name "$(L "Имя (латиница, цифры, . _ -)" "Name (Latin letters, digits, . _ -)")" "steve"
		valid_name "$name" && break
		warn "$(L "«$name» не подходит: от 1 до 32 символов A-Z a-z 0-9 . _ -" "\"$name\" will not do: 1 to 32 characters A-Z a-z 0-9 . _ -")"
		interactive || exit 1
		name=""
	done

	step 2 "$(L "адрес сервера" "server address")"
	if [[ -z $address ]]; then
		info "$(L "Узнаю внешний IP…" "Looking up the public IP…")"
		detected=$(detect_ip || true)
		if [[ -n $detected ]]; then
			ok "$(L "внешний IP: $detected" "public IP: $detected")"
		else
			warn "$(L "Внешний IP узнать не удалось." "Could not find the public IP.")"
		fi
		hint "$(L "Этот адрес вы впишете в мод, по нему же зайдут друзья. Можно указать домен." \
			"You enter this address in the mod, and friends join through it. A domain works too.")"
		while :; do
			ask address "$(L "Адрес для мода" "Address for the mod")" "$detected"
			valid_address "$address" && break
			warn "$(L "Нужен IP-адрес или домен (латиница, цифры, точки и дефисы)." \
				"An IP address or a domain is needed (Latin letters, digits, dots and hyphens).")"
			interactive || exit 1
		done
	fi
	valid_address "$address" || die "$(L "Адрес «$address» не подходит." "The address \"$address\" will not do.")"

	step 3 "$(L "порты и файрвол" "ports and firewall")"
	if [[ -n $OPT_PORTS ]]; then
		local range
		read -r tcp tls kcp range <<<"$OPT_PORTS"
		pmin=${range%-*} pmax=${range#*-}
	else
		if listening tcp | grep -qx 443; then
			warn "$(L "Порт 443 уже занят (веб-сервером?): TLS будет выключен, TCP и KCP работают." \
				"Port 443 is taken (by a web server?): TLS stays off, TCP and KCP work.")"
			tls=0
		fi
		printf '    %s%s%s %s\n' "$WHITE" "$(pad "TCP $tcp" 18)" "$R" "$(L "подключение хостов (из мода)" "hosts connect here (from the mod)")"
		if ((tls != 0)); then
			printf '    %s%s%s %s\n' "$WHITE" "$(pad "TCP $tls" 18)" "$R" "$(L "TLS: для сетей, где режут необычные порты" "TLS: for networks that block unusual ports")"
		fi
		printf '    %s%s%s %s\n' "$WHITE" "$(pad "UDP $kcp" 18)" "$R" "$(L "KCP: для Wi-Fi и мобильного интернета с потерями" "KCP: for Wi-Fi and mobile internet with packet loss")"
		printf '    %s%s%s %s\n' "$WHITE" "$(pad "TCP $pmin–$pmax" 18)" "$R" "$(L "друзья заходят в миры (порт на пользователя)" "friends join the worlds (one port per user)")"
		if ! confirm "$(L "Оставить эти порты?" "Keep these ports?")" y; then
			while :; do
				ask tcp "$(L "TCP-порт для хостов" "TCP port for hosts")" "$tcp"
				valid_port "$tcp" && break
				warn "$(L "Порт — число от 1 до 65535." "A port is a number from 1 to 65535.")"
			done
			while :; do
				ask tls "$(L "TLS-порт (0 — без TLS)" "TLS port (0: no TLS)")" "$tls"
				[[ $tls == 0 ]] || valid_port "$tls" && break
				warn "$(L "Порт — число от 1 до 65535 или 0." "A port is a number from 1 to 65535, or 0.")"
			done
			while :; do
				ask kcp "$(L "UDP-порт для KCP (0 — без KCP)" "UDP port for KCP (0: no KCP)")" "$tcp"
				[[ $kcp == 0 ]] || valid_port "$kcp" && break
				warn "$(L "Порт — число от 1 до 65535 или 0." "A port is a number from 1 to 65535, or 0.")"
			done
			while :; do
				ask pmin "$(L "Первый порт для друзей (по одному на пользователя, всего 100)" "First port for friends (one per user, 100 in all)")" "$pmin"
				valid_port "$pmin" && ((pmin + 99 <= 65535)) && break
				warn "$(L "Нужен порт от 1 до 65436." "A port from 1 to 65436 is needed.")"
			done
			pmax=$((pmin + 99))
		fi
	fi
	local busy
	busy=$(listening tcp | awk -v a="$tcp" -v b="$tls" -v lo="$pmin" -v hi="$pmax" '$1 == a || $1 == b || ($1 >= lo && $1 <= hi)' | head -n 3 | tr '\n' ' ')
	[[ -z $busy ]] || die "$(L "Порты уже заняты другой программой: $busy" "These ports are taken by another program: $busy")" \
		"$(L "Запустите установщик ещё раз и выберите другие порты." "Run the installer again and choose other ports.")"
	if ((kcp != 0)) && listening udp | grep -qx "$kcp"; then
		die "$(L "UDP-порт $kcp уже занят другой программой." "UDP port $kcp is taken by another program.")" \
			"$(L "Запустите установщик ещё раз и выберите другой порт." "Run the installer again and choose another port.")"
	fi
	local open=("$tcp/tcp" "$pmin-$pmax/tcp") firewall=0
	((tls == 0)) || open+=("$tls/tcp")
	((kcp == 0)) || open+=("$kcp/udp")
	printf '\n'
	if firewall_step "${open[@]}"; then firewall=1; fi

	printf '\n'
	confirm "$(L "Устанавливаю relay для «$name» на $address?" "Install the relay for \"$name\" on $address?")" y ||
		{ info "$(L "Отменено, ничего не изменено." "Cancelled, nothing was changed.")"; exit 0; }

	title "$(L "Установка" "Installing")"
	stage_binary
	ensure_sysuser
	"$BIN.new" init -config "$CONF" -user "$name" -tcp "$tcp" -tls "$tls" -kcp "$kcp" -players "$pmin-$pmax" >"$WORK/user" ||
		die "$(L "Не удалось создать настройки $CONF." "Could not create the configuration $CONF.")"
	chgrp "$SYSUSER" "$CONF"
	chmod 0640 "$CONF"
	ok "$(L "настройки: $CONF" "configuration: $CONF")"
	mv -f "$BIN.new" "$BIN"
	ok "relay: $BIN"
	save_address "$address"
	state_set LANGUAGE "$UI"
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
		die "$(L "Relay не запустился." "The relay did not start.")" \
			"$(L "Причина — в строках выше. Исправьте её и запустите установщик ещё раз." "The reason is in the lines above. Fix it and run the installer again.")"
	fi
	ok "$(L "служба $SERVICE работает и запустится сама после перезагрузки" "the $SERVICE service runs and starts by itself after a reboot")"

	toast "$(L "Свой relay-сервер" "A relay of your own")"
	card "$WORK/user"
	provider_reminder
	printf '\n  %s%s%s\n\n' "$GREEN$BOLD" "$(L "Готово! Откройте мир для сети — адрес для друзей появится в чате." \
		"Done! Open your world to LAN: the address for friends appears in the chat.")" "$R"
}

# update: the new binary (and unit, sysctl file) with the installed configuration, or with
# $NEW_CONFIG. Also the first installation from deploy.ps1 with -Config.
action_update() {
	local before after cfg backup="" had_unit=0 bin_changed=0 unit_changed=0
	cfg=${NEW_CONFIG:-$CONF}
	if installed; then
		title "$(L "Обновление relay" "Updating the relay")"
	else
		title "$(L "Установка relay" "Relay installation")"
	fi
	before=$([[ -x $BIN ]] && relay_version "$BIN" || true)
	stage_binary
	after=$(relay_version "$BIN.new")
	ensure_sysuser
	if ! "$BIN.new" -config "$cfg" -check >"$WORK/check" 2>&1; then
		sed 's/^/    /' "$WORK/check" >&2
		die "$(L "Новый relay не принимает настройки $cfg." "The new relay does not accept the configuration $cfg.")" \
			"$(L "Ничего не изменено." "Nothing was changed.")"
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
			info "$(L "прежние настройки сохранены: $backup" "the previous configuration is saved as $backup")"
		fi
		install -m 0640 -o root -g "$SYSUSER" "$NEW_CONFIG" "$CONF.new"
		mv -f "$CONF.new" "$CONF"
		ok "$(L "настройки: $CONF" "configuration: $CONF")"
	elif [[ $(stat -c %U:%G:%a "$CONF") != "root:$SYSUSER:640" ]]; then
		# Written by hand (nano, cp without -p): the relay runs as $SYSUSER and must read it,
		# nobody else should.
		chown "root:$SYSUSER" "$CONF"
		chmod 0640 "$CONF"
		info "$(L "$CONF: владелец root:$SYSUSER, права 0640" "$CONF: owner root:$SYSUSER, mode 0640")"
	fi
	state_set LANGUAGE "$UI"
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
		ok "$(L "уже установлена версия $after, relay работает" "version $after is already installed, the relay is running")"
		return 0
	fi
	info "$(L "Перезапускаю relay: туннели переподключатся сами за несколько секунд." \
		"Restarting the relay: tunnels reconnect by themselves within seconds.")"
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	systemctl restart "$SERVICE" || true
	if relay_up; then
		if [[ -n $before && $before != "$after" ]]; then
			ok "$(L "relay обновлён: $before → $after, работает" "relay updated: $before → $after, running")"
		else
			ok "$(L "relay $after работает" "relay $after is running")"
		fi
		return 0
	fi

	show_journal 12
	warn "$(L "Relay не остался запущенным: возвращаю прежнюю версию." "The relay did not stay up: putting the previous version back.")"
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
			die "$(L "Обновление не удалось, прежняя версия снова работает." "The update failed; the previous version runs again.")" \
				"$(L "Причина — в журнале выше." "The reason is in the log above.")"
		fi
	fi
	die "$(L "Relay не запускается." "The relay does not start.")" \
		"$(L "Причина — в журнале выше:" "The reason is in the log above:") sudo journalctl -u $SERVICE -n 50"
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
		printf '    %s%d%s  %s  %s(%s %s%s)%s\n' "$AQUA$BOLD" "$i" "$R" "$n" "$DGRAY" "$(L "порт" "port")" "$p" \
			"$([[ $s == off ]] && L ", выключен" ", off" || true)" "$R"
		i=$((i + 1))
	done
	interactive || return 1
	ask answer "$1 $(L "(номер или имя, Enter — отмена)" "(number or name, Enter: cancel)")" ""
	[[ -n $answer ]] || return 1
	if [[ $answer =~ ^[0-9]+$ ]] && ((answer >= 1 && answer <= ${#lines[@]})); then
		PICKED=${lines[answer - 1]%%$'\t'*}
		return 0
	fi
	for line in "${lines[@]}"; do
		[[ ${line%%$'\t'*} == "$answer" ]] && PICKED=$answer && return 0
	done
	warn "$(L "Нет такого пользователя: $answer" "No such user: $answer")"
	return 1
}

action_show() {
	local name=${1:-} address detected
	require_manageable || return 1
	if [[ -z $(saved_address) ]]; then
		detected=$(detect_ip || true)
		ask address "$(L "Адрес сервера для мода" "Server address for the mod")" "$detected"
		if valid_address "$address"; then save_address "$address"; fi
	fi
	if [[ -z $name ]]; then
		pick_user "$(L "Чьи данные показать?" "Whose data to show?")" || return 0
		name=$PICKED
	fi
	"$BIN" user show -config "$CONF" "$name" >"$WORK/user" 2>"$WORK/err" || { warn "$(cat "$WORK/err")"; return 1; }
	card "$WORK/user"
}

action_add() {
	local name=${1:-} err
	title "$(L "Новый пользователь" "New user")"
	hint "$(L "Каждый, кто открывает через этот relay свой мир, — отдельный пользователь со своим" \
		"Everyone who opens their world through this relay is a separate user with their own")"
	hint "$(L "секретом и своим адресом для друзей." "secret and their own address for friends.")"
	require_manageable || return 1
	while :; do
		if [[ -z $name ]]; then
			interactive || die "$(L "Укажите имя: add ИМЯ" "Give the name: add NAME")"
			ask name "$(L "Имя (латиница, цифры, . _ -; Enter — отмена)" "Name (Latin letters, digits, . _ -; Enter: cancel)")" ""
			[[ -n $name ]] || return 0
		fi
		if ! valid_name "$name"; then
			warn "$(L "«$name» не подходит: от 1 до 32 символов A-Z a-z 0-9 . _ -" "\"$name\" will not do: 1 to 32 characters A-Z a-z 0-9 . _ -")"
		elif "$BIN" user show -config "$CONF" "$name" >/dev/null 2>&1; then
			warn "$(L "Пользователь $name уже есть." "User $name already exists.")"
		else
			break
		fi
		interactive || exit 1
		name=""
	done
	if ! "$BIN" user add -config "$CONF" "$name" >"$WORK/user" 2>"$WORK/err"; then
		err=$(cat "$WORK/err")
		warn "$(L "Не получилось: $err" "That did not work: $err")"
		return 1
	fi
	ok "$(L "пользователь $name добавлен" "user $name added")"
	reload_relay
	card "$WORK/user"
}

action_remove() {
	local name=${1:-} answer count
	title "$(L "Удаление пользователя" "Removing a user")"
	require_manageable || return 1
	count=$("$BIN" user list -config "$CONF" | wc -l)
	if ((count <= 1)); then
		warn "$(L "Это единственный пользователь: relay без пользователей не работает." "This is the only user: the relay does not work without users.")"
		hint "$(L "Сначала добавьте другого или удалите MCTunnel целиком." "Add another one first, or uninstall MCTunnel altogether.")"
		return 1
	fi
	if [[ -z $name ]]; then
		pick_user "$(L "Кого удалить?" "Whom to remove?")" || return 0
		name=$PICKED
	fi
	if ((OPT_YES == 0)); then
		interactive || die "$(L "Для удаления без вопросов: remove ИМЯ --yes" "To remove without questions: remove NAME --yes")"
		warn "$(L "Туннель $name закроется, его друзья отключатся, секрет перестанет работать." \
			"The tunnel of $name closes, their friends get disconnected, the secret stops working.")"
		ask answer "$(L "Чтобы подтвердить, введите имя ещё раз" "To confirm, type the name again")" ""
		[[ $answer == "$name" ]] || { info "$(L "Отменено." "Cancelled.")"; return 0; }
	fi
	if ! "$BIN" user remove -config "$CONF" "$name" 2>"$WORK/err"; then
		warn "$(L "Не получилось: $(cat "$WORK/err")" "That did not work: $(cat "$WORK/err")")"
		return 1
	fi
	ok "$(L "пользователь $name удалён" "user $name removed")"
	reload_relay
}

action_list() {
	local line n p s
	title "$(L "Пользователи" "Users")"
	require_manageable || return 1
	while IFS=$'\t' read -r n p s; do
		printf '    %s  %s%s %s%s%s\n' "$(pad "$n" 20)" "$DGRAY" "$(L "порт" "port")" "$p" \
			"$([[ $s == off ]] && L ", выключен" ", off" || true)" "$R"
	done < <("$BIN" user list -config "$CONF")
}

action_firewall() {
	title "$(L "Файрвол" "Firewall")"
	ports_of
	if ! firewall_step "${HOST_PORTS[@]}" "$PLAYER_RANGE/tcp"; then
		info "$(L "Файрвол не трогаю." "Leaving the firewall alone.")"
		return 0
	fi
	lockdown "${FIREWALL_KEEP[@]}"
}

action_restart() {
	title "$(L "Перезапуск relay" "Restarting the relay")"
	info "$(L "Туннели переподключатся сами за несколько секунд." "Tunnels reconnect by themselves within seconds.")"
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	systemctl restart "$SERVICE" || true
	if relay_up; then
		ok "$(L "relay работает" "the relay is running")"
	else
		show_journal 12
		warn "$(L "Relay не запустился: причина — в строках выше." "The relay did not start: the reason is in the lines above.")"
	fi
}

action_log() {
	title "$(L "Журнал relay" "Relay log")"
	show_journal 25
	hint "$(L "Весь журнал:" "The whole log:") sudo journalctl -u $SERVICE -f"
}

action_uninstall() {
	local answer
	title "$(L "Удаление MCTunnel" "Uninstalling MCTunnel")"
	warn "$(L "Relay остановится, друзья отключатся, настройки с секретами будут удалены." \
		"The relay stops, friends get disconnected, the configuration with the secrets is deleted.")"
	if ((OPT_YES == 0)); then
		interactive || die "$(L "Для удаления без вопросов: uninstall --yes" "To uninstall without questions: uninstall --yes")"
		ask answer "$(L "Чтобы подтвердить, введите: удалить" "To confirm, type: delete")" ""
		case "$answer" in
			удалить | Удалить | УДАЛИТЬ | delete | Delete | DELETE) ;;
			*) info "$(L "Отменено." "Cancelled.")"; return 1 ;;
		esac
	fi
	[[ -x $BIN ]] && close_firewall
	systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
	rm -f "$UNIT"
	systemctl daemon-reload
	systemctl reset-failed "$SERVICE" >/dev/null 2>&1 || true
	ok "$(L "служба $SERVICE остановлена и удалена" "the $SERVICE service is stopped and removed")"
	rm -f "$BIN" "$BIN.prev" "$BIN.new"
	rm -rf "$ETC"
	ok "$(L "удалены $BIN и $ETC" "removed $BIN and $ETC")"
	rm -f "$SYSCTL" "$MODLOAD"
	userdel "$SYSUSER" >/dev/null 2>&1 || true
	groupdel "$SYSUSER" >/dev/null 2>&1 || true
	ok "$(L "удалён системный пользователь $SYSUSER" "removed the system user $SYSUSER")"
	hint "$(L "BBR остаётся включённым до перезагрузки сервера." "BBR stays on until the server reboots.")"
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		hint "$(L "Файрвол остаётся включённым: порты relay закрыты, SSH открыт (выключить: sudo ufw disable)." \
			"The firewall stays on: the relay's ports are closed, SSH is open (to turn it off: sudo ufw disable).")"
	fi
	printf '\n  %s%s%s\n\n' "$GREEN$BOLD" "$(L "MCTunnel удалён. Спасибо, что играли!" "MCTunnel is uninstalled. Thanks for playing!")" "$R"
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
		dot="$GREEN●$R" state=$(L "работает" "running")
	else
		dot="$RED●$R" state=$(L "остановлен" "stopped")
	fi
	users="?"
	if manageable; then users=$("$BIN" user list -config "$CONF" 2>/dev/null | wc -l); fi
	address=$(saved_address)
	printf '\n  %s %srelay %s%s · %s · %s %s%s\n' "$dot" "$WHITE$BOLD" "${version:-?}" "$R" "$state" \
		"$(L "пользователей:" "users:")" "$users" "$([[ -n $address ]] && printf ' · %s' "$address" || true)"
}

menu() {
	local choice target=$RELEASE
	[[ $target == dev ]] && target=$(L "последней версии" "the latest version")
	state_set LANGUAGE "$UI"
	while :; do
		status_line
		printf '\n'
		printf '    %s1%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Обновить relay до $target" "Update the relay to $target")"
		printf '    %s2%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Показать данные для мода" "Show the data for the mod")"
		printf '    %s3%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Добавить пользователя" "Add a user")"
		printf '    %s4%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Удалить пользователя" "Remove a user")"
		printf '    %s5%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Перезапустить relay" "Restart the relay")"
		printf '    %s6%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Журнал relay" "Relay log")"
		printf '    %s7%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Файрвол: закрыть всё, кроме relay и SSH" "Firewall: close everything but the relay and SSH")"
		printf '    %s8%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Удалить MCTunnel с сервера" "Uninstall MCTunnel from the server")"
		printf '    %s0%s  %s\n' "$AQUA$BOLD" "$R" "$(L "Выход" "Exit")"
		interactive || return 0
		ask choice "$(L "Выберите" "Choose")" "0"
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
			*) warn "$(L "Нет такого пункта: $choice" "No such item: $choice")" ;;
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
  --lang ru|en                      the language, without asking
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
			--lang) OPT_LANG=${2:-}; shift ;;
			--user) OPT_USER=${2:-}; shift ;;
			--address) OPT_ADDRESS=${2:-}; shift ;;
			--ports) OPT_PORTS=${2:-}; shift ;;
			--keep) OPT_KEEP=${2:-}; shift ;;
			--no-firewall) OPT_NO_FIREWALL=1 ;;
			--yes | -y) OPT_YES=1 ;;
			-h | --help | help) usage; exit 0 ;;
			*) usage >&2; die "$(L "Непонятный аргумент: $1" "Unknown argument: $1")" ;;
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
	choose_language
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
			installed && die "$(L "Relay уже установлен." "The relay is already installed.")" \
				"$(L "Запустите установщик без аргументов: откроется меню." "Run the installer without arguments: the menu opens.")"
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
