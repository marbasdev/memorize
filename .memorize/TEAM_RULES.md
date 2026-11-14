# Team Rules (`memorize`)

> Shared repository rules managed via Git. Mirrored read-only into local `memorize` daemons.

## Architecture (`architecture`)

- (`engine`) internal/domain 패키지는 다른 internal/* 패키지를 임포트하지 않는 순수 도메인 타입 및 정규화 함수 계층으로 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`engine`) internal/modules/* 패키지는 internal/transport/*나 internal/app/*를 임포트하지 않으며, CLI·HTTP·MCP·대시보드 전달 계층이 modules와 inference를 호출하는 단방향 의존성을 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`workflow`) 프로덕션 코드의 git 명령 호출은 internal/infra/git 내부에만 두고 modules나 transport에서 직접 실행하지 않습니다. · `@memorize-dev` · `2026-11-14`
- (`database`) SQLite DB 연결과 스키마 초기화는 internal/infra/sqlite의 Open과 InitSchema로 일원화하고 팀 룰 미러 테이블은 internal/modules/rulepack 외 경로에서 임의 변경하지 않습니다. · `@memorize-dev` · `2026-11-14`
- (`dashboard`) 서드파티 번들을 제외한 internal/transport/dashboard/assets 및 templates 커스텀 파일은 각 800줄 미만 모듈로 분리 유지합니다. · `@memorize-dev` · `2026-11-14`

## Convention (`convention`)

- (`http`) internal/transport/http의 JSON POST 엔드포인트는 수동 헤더 검사 대신 security.go의 미들웨어와 유틸리티를 사용해 루프백 Host/Origin 및 Sec-Fetch-Site 검증을 일관되게 적용합니다. · `@memorize-dev` · `2026-11-14`
