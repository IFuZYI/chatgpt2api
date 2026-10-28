import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import type { NextConfig } from 'next'
import { parseChangelog } from './src/lib/release'

const projectRoot = join(dirname(fileURLToPath(import.meta.url)), '..')

function readAppVersion() {
    try {
        const version = readFileSync(join(projectRoot, 'VERSION'), 'utf-8').trim()
        return version || '0.0.0'
    } catch {
        return '0.0.0'
    }
}

const appVersion = process.env.NEXT_PUBLIC_APP_VERSION || readAppVersion()
let appReleases = '[]'
try {
    appReleases = JSON.stringify(parseChangelog(readFileSync(join(projectRoot, 'CHANGELOG.md'), 'utf-8')))
} catch {}

// 用「版本 + 提交号」当构建 ID。
//
// 为什么不能用 Next.js 默认的随机 ID：它每次构建都不一样，于是前端产物
// 不可复现 —— 而前端是 embed 进二进制的，随机 ID 会让同一份源码两次构建
// 产出不同字节的发布包（实测：_next/static/<随机ID>/ 目录名变化，
// 连带 404.html、index.html 等所有引用它的文件都变）。
//
// 为什么不能用常量：build ID 参与客户端缓存失效。跨版本复用同一 ID 会让
// 浏览器继续用旧的 manifest。所以取「版本 + 提交号」：同源码同 ID（可复现），
// 不同提交不同 ID（缓存正确）。
//
// 提交号来源依次为 SOURCE_COMMIT / GITHUB_SHA / 本地 git。Docker 构建时
// .git 不在上下文里，退化为「版本-nogit」—— 仍是确定的，且随版本变化。
function resolveSourceCommit(): string {
    const fromEnv = (process.env.SOURCE_COMMIT || process.env.GITHUB_SHA || '').trim()
    if (fromEnv) return fromEnv
    try {
        return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: projectRoot, stdio: ['ignore', 'pipe', 'ignore'] })
            .toString().trim()
    } catch {
        return 'nogit'
    }
}

function deterministicBuildId(): string {
    return `${appVersion}-${resolveSourceCommit().slice(0, 12)}`
}

const nextConfig: NextConfig = {
    generateBuildId: deterministicBuildId,
    allowedDevOrigins: ['127.0.0.1'],
    env: {
        NEXT_PUBLIC_APP_VERSION: appVersion,
        NEXT_PUBLIC_APP_RELEASES: appReleases,
    },
    output: 'export',
    trailingSlash: true,
    images: {
        unoptimized: true,
    },
    typescript: {
        ignoreBuildErrors: true,
    },
}

export default nextConfig
