# 팀 협업 & 공통 룰 운영 가이드

[English (TEAM.md)](./TEAM.md) · [README로 돌아가기](./README.ko.md) · [아키텍처 레퍼런스](./ARCHITECTURE.ko.md)

팀이 합의한 공통 규칙은 저장소의 **`.memorize/TEAM_RULES.md`**(Git)로 공유하고, 개인 규칙과 작업 중인 로컬 기억은 각자의 로컬 DB에 분리해 유지합니다.

---

## 1. 동작 방식

- **개인 · 작업 중 기억은 로컬 유지**: 각자 PC의 로컬 데몬(`127.0.0.1:18095`)과 `~/.memorize/memories.db`는 공유되지 않으므로 개인 취향이나 미완성 브랜치 결정이 팀원 세션에 섞이지 않습니다.
- **변경 자동 감지 → 메모리 반영**: 채팅 턴과 훅 실행 시(또는 **`Sync Remote`** · `team_rules(action="sync")` 실행 시) **`.memorize/TEAM_RULES.md`** 변경을 감지해 조회·가드레일(`PreToolUse` · `Stop` · `git pre-commit`)에 자동 반영합니다. 내 로컬 기억과 충돌할 때는 임의로 덮어쓰지 않고 멈춰 선택(`adopt_team` · `local_override` · `mark_updated`)을 받습니다.

---

## 2. 필수 수칙: 반드시 대시보드 또는 MCP 명령으로만 수정

> [!IMPORTANT]
> **`.memorize/TEAM_RULES.md` 파일을 직접 손으로 편집하지 마세요.**
> 공유 규칙 추가·승격·수정·삭제는 반드시 **웹 콘솔** 또는 **MCP `team_rules`** 명령으로만 처리해야 `memorize`가 격리된 Git 워크트리에서 `.memorize/TEAM_RULES.md`만 깔끔하게 커밋·푸시해 작업 중인 브랜치 코드와 섞이지 않습니다.

- **웹 콘솔 (`http://127.0.0.1:18095`)**:
  - **Memories 탭 → `Promote`**: `project:*` 로컬 기억을 깔끔한 1줄 팀 규칙으로 승격
  - **Team Rules 탭**: 규칙 추가·수정·삭제, 원격 동기화(`Sync Remote`), 중복 로컬 기억 정리(`Check Duplicates`), `@git-handle` 설정, 충돌 해결
- **MCP 도구 (`team_rules`)**:
  - `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`
- **원격 반영 방식**: 모든 변경은 임시 `git worktree`에서 `.memorize/TEAM_RULES.md`만 단독 커밋해 대상 브랜치로 즉시 푸시하며, 동시 수정 충돌 시 최신본을 기준으로 안전하게 재시도합니다 *(보호 브랜치에서 푸시가 거부되면 에러를 반환합니다)*.

---

*참고: 팀원 간 `project:<repo>` 스코프가 일치하도록 클론 폴더명을 맞추고, 필요 시 `memorize -install git-hook`으로 커밋 전 검사를 함께 쓸 수 있습니다. 1인 다기기 간 동기화는 `v1.1`에서는 비활성 상태이며 추후 로드맵에서 지원할 예정입니다.*
