import { lazy, Suspense } from 'react'
import type { RouteConfig } from '../types'

// 懒加载窗口组件（仅保留全局搜索窗口框架）
const SearchWindow = lazy(() => import('../../windows/SearchWindow'))

/**
 * 加载中组件（窗口版本 - 全屏）
 */
function WindowLoadingFallback() {
  return (
    <div className="flex items-center justify-center h-screen w-screen bg-[#0D0F1A]">
      <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-[#7C3AED]"></div>
    </div>
  )
}

/**
 * 懒加载包装器
 */
function LazyWindowWrapper({ children }: { children: React.ReactNode }) {
  return (
    <Suspense fallback={<WindowLoadingFallback />}>
      {children}
    </Suspense>
  )
}

/**
 * 独立窗口路由配置
 * 这些路由不包含侧边栏，是独立的窗口视图
 */
export const windowRoutes: RouteConfig[] = [
  {
    path: '/search',
    element: (
      <LazyWindowWrapper>
        <SearchWindow />
      </LazyWindowWrapper>
    ),
  },
]

/**
 * 判断当前路径是否为窗口路由
 */
export function isWindowPath(path: string): boolean {
  return path === '/search'
}
