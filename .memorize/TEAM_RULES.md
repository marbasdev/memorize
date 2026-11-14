# Team Rules (`memorize`)

> Shared repository rules managed via Git. Mirrored read-only into local `memorize` daemons.

## Architecture (`architecture`)

- (`engine`) internal/domain 패키지는 다른 internal/* 패키지를 임포트하지 않는 순수 도메인 타입 및 정규화 함수 계층으로 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`engine`) internal/modules/* 패키지는 internal/transport/*나 internal/app/*를 임포트하지 않으며, CLI·HTTP·MCP·대시보드 전달 계층이 modules와 inference를 호출하는 단방향 의존성을 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`workflow`) 프로덕션 코드의 git 명령 호출은 internal/infra/git 내부에만 두고 modules나 transport에서 직접 실행하지 않습니다. · `@memorize-dev` · `2026-11-14`
