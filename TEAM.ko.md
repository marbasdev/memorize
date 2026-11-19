# 팀 협업 & 공통 룰 운영 가이드

[English (TEAM.md)](./TEAM.md) · [README로 돌아가기](./README.ko.md) · [아키텍처 레퍼런스](./ARCHITECTURE.ko.md)

`memorize`는 두 가지 팀 협업 방식을 지원합니다. 로컬 데몬과 **Git 기반 `.memorize/TEAM_RULES.md`**만 조합해 쓰거나, **공유 팀 서버 모드(`memorize serve`)**를 띄워 하나의 SQLite 서버·추론 엔진을 팀 전체가 함께 사용할 수 있습니다.

---

## 1. Git 기반 선언적 팀 룰팩 (`.memorize/TEAM_RULES.md`)

- **변경 자동 감지 → 메모리 반영**: 채팅 턴과 훅 실행 시(또는 **`Sync Remote`** · `team_rules(action="sync")` 실행 시) **`.memorize/TEAM_RULES.md`** 변경을 감지해 조회·가드레일(`PreToolUse` · `Stop` · `git pre-commit`)에 자동 반영합니다.
- **필수 수칙 — 반드시 대시보드 또는 MCP `team_rules`로만 수정**:

> [!IMPORTANT]
> **`.memorize/TEAM_RULES.md` 파일을 직접 손으로 편집하지 마세요.**
> 공유 규칙 추가·승격·수정·삭제는 반드시 **웹 콘솔** 또는 **MCP `team_rules`** 명령으로만 처리해야 `memorize`가 임시 `git worktree`에서 `.memorize/TEAM_RULES.md`만 단독 커밋·푸시해 작업 중인 브랜치 코드와 섞이지 않습니다 *(보호 브랜치에서 푸시가 거부되면 에러를 반환합니다)*.

- **웹 콘솔**:
  - **Memories 탭 → `Promote`**: `project:*` 로컬 기억을 깔끔한 1줄 팀 규칙으로 승격
  - **Team Rules 탭**: 규칙 추가·수정·삭제, 원격 동기화(`Sync Remote`), 중복 로컬 기억 정리(`Check Duplicates`), `@git-handle` 설정, 충돌 해결
- **MCP 도구 (`team_rules`)**: `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`

---

## 2. 공유 팀 서버 모드 (`v1.2+` · `memorize serve` & `memorize connect`)

단일 바이너리와 SQLite(`WAL` 동시 조회 풀)만으로 인증된 팀 서버를 실행하고, 팀원 PC는 **로컬 DB나 GGUF 모델 없이** 가벼운 씬 클라이언트(Thin Client)로 연결할 수 있습니다.

### 서버 기동 및 계정 관리

```bash
# 1. 인증된 팀 서버 실행 (기본 0.0.0.0:18095)
#    최초 실행 시 초기 관리자(admin) 액세스 토큰을 1회 출력합니다.
memorize serve --addr 0.0.0.0:18095

# 2. 팀원 토큰 발급 및 관리 (웹 콘솔 상단 'Accounts' 모달에서도 가능)
memorize user add alice --role member
memorize user list
memorize user rotate alice
memorize user disable alice
```

> [!TIP]
> 사내망 외부로 `memorize serve`를 노출할 때는 루프백 또는 사설망 리버스 프록시(Caddy · Nginx · Traefik) 뒤에서 HTTPS(TLS)를 종단해 세션 쿠키(`Secure`)와 Bearer 토큰을 보호하세요.

### 팀원 클라이언트 연결

CLI 또는 로컬 웹 콘솔 상단의 **`Remote Servers`** 버튼으로 연결할 수 있습니다:

```bash
# 전체 저장소에 기본 팀 서버 연결
memorize connect --url https://memorize.company.com --token mem_...

# 특정 저장소만 팀 서버에 연결 (나머지 프로젝트는 로컬 데몬 유지)
memorize connect --url https://memorize.company.com --token mem_... --repo .

# 연결 해제
memorize disconnect          # 또는: memorize disconnect --repo .
```

- **호스트 파일시스템 비접근**: 팀원의 Git diff, 저장소 루트, `.memorize/TEAM_RULES.md` 스냅샷은 클라이언트 측에서 수집해 요청에 실어 보내며, 팀 서버는 호스트 파일시스템을 탐색하지 않습니다.
- **일반 MCP 클라이언트 직접 연결**: 훅 없는 클라이언트는 `https://<host>/mcp`에 `Authorization: Bearer <token>` 헤더로 직접 연결할 수 있습니다 (**Accounts** 모달에서 JSON 설정 자동 생성).

### 4계층 규칙 우선순위 및 계정별 격리 (팀 서버 모드)

팀 서버 모드에서는 **팀 공통 표준이 항상 최우선**으로 적용되면서도 팀원 간 공유 KV 캐시 효율을 극대화하도록 4계층으로 격리·정렬합니다:

| 우선순위 | 스코프 | 가시성 및 수정 권한 |
| :--- | :--- | :--- |
| **1 (최우선)** | **팀 룰팩** (`.memorize/TEAM_RULES.md`) | 팀 전체 공통 표준; 서버 모드에서는 개인 예외(`local_override`) 비활성화 |
| **2** | **`project:<repo>`** | 팀 전체 공유 프로젝트 규칙 · 진행 현황 (`project:*`를 `global`/`personal`로 이동 차단) |
| **3** | **`global`** | **계정별 격리** — 각 팀원의 전 프로젝트 공통 개발 규칙 (모든 인증 팀원이 본인 것 자유 수정) |
| **4** | **`personal`** | **계정별 격리** — 개발 외 개인 선호·메모 (프로젝트 조회 시 선택적 오버레이) |

- **역할 기반 권한(RBAC)**: `admin`은 추론 엔진·임계값·보관 정책·계정 관리를 담당하고, `member`는 `project:*` 협업과 본인 `global`·`personal` 규칙을 관리합니다.
- **멀티 유저 공유 KV 프리픽스 캐시**: 기억 조회와 가드레일 평가 모두 활성 규칙을 **`TEAM` → `project:*` → `global` → `personal`** 순으로 정렬해 동시 접속 팀원들이 동일한 정적 프리픽스 KV 캐시를 공유합니다.

---

*참고: `origin` 원격 저장소명이 같으면 로컬 클론 폴더명이 달라도 동일한 `project:<repo>` 스코프로 자동 일치합니다. 필요 시 `memorize -install git-hook`으로 로컬 커밋 전 검사도 함께 사용할 수 있습니다.*
