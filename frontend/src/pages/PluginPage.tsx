import { useParams, useNavigate } from 'react-router-dom'
import { useMemo } from 'react'
import { Icon } from '../components/Icon'
import { usePlugins } from '../plugins/usePlugins'
import { getPluginIcon } from '../utils/pluginHelpers'
import { Col1688Widget } from '../components/Col1688Widget'
import { t } from '@/i18n';

/**
 * 插件页面组件
 * 支持状态缓存（KeepAlive）
 */
function PluginPage() {
  const { pluginId } = useParams<{ pluginId: string }>()
  const navigate = useNavigate()
  const { plugins, loading } = usePlugins()

  const plugin = useMemo(() => {
    return plugins.find(p => p.id === pluginId)
  }, [plugins, pluginId])

  const handleBack = () => {
    navigate('/')
  }

  // 加载中显示骨架屏
  if (loading) {
    return (
      <div className="p-8">
        <div className="max-w-4xl mx-auto">
          <div className="animate-pulse">
            <div className="text-center mb-8">
              <div className="inline-flex items-center justify-center w-20 h-20 rounded-2xl bg-white/10 mb-6" />
              <div className="h-8 bg-white/10 rounded w-48 mx-auto mb-2" />
              <div className="h-4 bg-white/10 rounded w-64 mx-auto" />
            </div>
            <div className="glass-light rounded-xl p-8">
              <div className="h-32 bg-white/10 rounded" />
            </div>
          </div>
        </div>
      </div>
    )
  }

  if (!plugin) {
    return (
      <div className="p-8 text-center">
        <div className="inline-flex items-center justify-center w-16 h-16 rounded-full bg-[#EF4444]/10 mb-4">
          <Icon name="exclamation-circle" size={32} color="#EF4444" />
        </div>
        <h2 className="text-xl font-semibold text-white mb-2">{t('插件未找到')}</h2>
        <p className="text-white/60">{t('插件 "')}{pluginId}{t('" 不存在或未启用')}</p>
      </div>
    )
  }

  // 处理 hasPage: false 的插件
  if (plugin.hasPage === false) {
    return (
      <div className="p-8">
        <div className="max-w-2xl mx-auto text-center">
          <div className="inline-flex items-center justify-center w-20 h-20 rounded-2xl bg-gradient-to-br from-[#7C3AED] to-[#A78BFA] mb-6">
            <Icon name="cube" size={36} color="white" />
          </div>
          <h1 className="text-3xl font-bold mb-2">{t(plugin.name)}</h1>
          <p className="text-white/50 mb-4">{t(plugin.description)}</p>
          <div className="glass-light rounded-xl p-6">
            <p className="text-white/60">
              {t('此插件通过快捷键或其他方式调用，无需独立页面。')}
            </p>
            {plugin.keywords && plugin.keywords.length > 0 && (
              <div className="mt-4">
                <p className="text-sm text-white/40 mb-2">{t('关键词：')}</p>
                <div className="flex flex-wrap justify-center gap-2">
                  {plugin.keywords.map((kw: string, i: number) => (
                    <span key={i} className="px-2 py-1 rounded bg-[#7C3AED]/10 text-[#A78BFA] text-xs border border-[#7C3AED]/20">
                      {kw}
                    </span>
                  ))}
                </div>
              </div>
            )}
          </div>
        </div>
      </div>
    )
  }

  const pluginIcon = getPluginIcon(plugin)

  // 渲染插件内容
  return (
    <PluginContent
      pluginId={pluginId!}
      plugin={plugin}
      pluginIcon={pluginIcon}
      onBack={handleBack}
      isActive={true}
    />
  )
}

/**
 * 插件内容组件
 * 根据 pluginId 渲染对应的插件界面
 */
interface PluginContentProps {
  pluginId: string
  plugin: any
  pluginIcon: string
  onBack: () => void
  isActive: boolean
}

function PluginContent({ pluginId, plugin, pluginIcon, onBack, isActive }: PluginContentProps) {
  // 使用 CSS hidden 保持组件状态（KeepAlive）
  const visibilityClass = isActive ? '' : 'hidden'

  // 根据插件 ID 渲染对应的组件
  const renderPluginWidget = () => {
    switch (pluginId) {
      case 'c1688.builtin':
        return (
          <div className="h-full">
            <Col1688Widget />
          </div>
        )

      default:
        // 默认插件界面
        return (
          <div className="p-8">
            <div className="max-w-4xl mx-auto">
              {/* 插件页头 */}
              <div className="text-center mb-8">
                <div className="inline-flex items-center justify-center w-20 h-20 rounded-2xl bg-gradient-to-br from-[#7C3AED] to-[#A78BFA] mb-6">
                  <span className="text-4xl">{pluginIcon}</span>
                </div>
                <h1 className="text-3xl font-bold mb-2">{t(plugin.name)}</h1>
                <p className="text-white/50">{t(plugin.description)}</p>
                <p className="text-sm text-white/30 mt-2">
                  v{plugin.version} · by {plugin.author}
                </p>
              </div>

              {/* 插件内容区域 */}
              <div className="glass-light rounded-xl p-8">
                <div className="text-center py-12">
                  <Icon name="cube" size={48} color="rgba(167, 139, 250, 0.3)" />
                  <p className="text-white/40 mt-4">{t('插件功能正在开发中...')}</p>
                  <p className="text-white/30 text-sm mt-2">
                    {t('此插件 (')}{plugin.id}{t(') 已成功启用，但尚未实现用户界面。')}
                  </p>
                </div>
              </div>

              {/* 插件信息 */}
              <div className="mt-6 grid grid-cols-1 md:grid-cols-2 gap-4">
                {plugin.permissions && plugin.permissions.length > 0 && (
                  <div className="glass-light rounded-xl p-4">
                    <h3 className="text-sm font-medium text-white/60 mb-2">{t('所需权限')}</h3>
                    <div className="flex flex-wrap gap-2">
                      {plugin.permissions.map((perm: string, i: number) => (
                        <span key={i} className="px-2 py-1 rounded bg-[#F59E0B]/10 text-[#F59E0B] text-xs border border-[#F59E0B]/20">
                          {perm}
                        </span>
                      ))}
                    </div>
                  </div>
                )}
                {plugin.keywords && plugin.keywords.length > 0 && (
                  <div className="glass-light rounded-xl p-4">
                    <h3 className="text-sm font-medium text-white/60 mb-2">{t('关键词')}</h3>
                    <div className="flex flex-wrap gap-2">
                      {plugin.keywords.map((kw: string, i: number) => (
                        <span key={i} className="px-2 py-1 rounded bg-[#7C3AED]/10 text-[#A78BFA] text-xs border border-[#7C3AED]/20">
                          {kw}
                        </span>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            </div>
          </div>
        )
    }
  }

  return (
    <div className={visibilityClass}>
      {renderPluginWidget()}
    </div>
  )
}

export default PluginPage
