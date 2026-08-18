# evolution-go-jupiter

Build customizada da Evolution API GO baseada na versão oficial `0.7.2`, criada para estabilizar o ciclo de vida das instâncias e reduzir problemas de reconexão e vazamento de conexões PostgreSQL.

## Base

- Upstream: `evolution-foundation/evolution-go`
- Versão base: `0.7.2`
- Patch principal: PR #154 — estabilização de lifecycle, reconexão e restauração de sessões
- Patch PostgreSQL: PR #174 — reutilização do pool PostgreSQL no `StartClient`

## Tag interna

`0.7.2-jupiter1`

## Build

O `Dockerfile` clona exatamente a tag `0.7.2` do upstream, aplica os patches selecionados e compila a Evolution API GO. Nenhuma alteração é feita no banco durante a build.

## Observação

Este repositório é uma build operacional própria. Os patches continuam pertencendo aos respectivos autores/upstream e devem ser removidos quando as correções equivalentes forem incorporadas oficialmente em uma versão estável da Evolution API GO.
