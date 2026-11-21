import { useState, useEffect, useRef, useMemo } from 'react'
import * as Col1688Service from '../../bindings/ltools/plugins/col1688/collect1688service'
import { Supplier } from '../../bindings/ltools/plugins/col1688/models'
import { Events } from '@wailsio/runtime'
import { Icon } from './Icon'
import { useToast } from '../hooks/useToast'
import { ToastContainer } from './Toast'
import { t } from '@/i18n'

/**
 * 1688 工厂/供应商采集组件（Wails 子窗口模式）
 *
 * 极简 UX：
 *   控制条：「打开 1688」+「采集当前页」+「导出 CSV」+「导出诊断」
 *   参数区：全部删掉（用户直接在子窗口里输入关键词/选类别/调地区）
 *   结果表：名称/电话/地址/主营 + 复制/导出
 *
 * 数据流：
 *   1. 点「打开 1688」→ Go 端开 Wails 子窗口（加载 1688 首页）
 *   2. 用户在子窗口里手动登录/搜索/调地区/拖滑块
 *   3. 点「采集当前页」→ Go 端对子窗口 ExecJS 注入 __ltoolsCollect → 子窗口把 HTML 通过
 *      Wails 事件 c1688:page-html 回传 → Go 端 parseHTML → 累积到结果表
 *   4. 翻页：用户在子窗口里手动点「下一页」，再点一次「采集当前页」
 *   5. 点「导出 CSV」导出
 */

type RunState = 'idle' | 'running' | 'paused' | 'stopped'

interface LogLine {
  time: string
  msg: string
}

export function Col1688Widget() {
  const { toasts, removeToast, success, error: showError, info } = useToast()

  // ---- 运行态 ----
  const [state, setState] = useState<RunState>('idle')
  const [results, setResults] = useState<Supplier[]>([])
  const [logs, setLogs] = useState<LogLine[]>([])
  const [exportPath, setExportPath] = useState('')
  const [windowOpen, setWindowOpen] = useState(false)
  const [collecting, setCollecting] = useState(false)
  const [autoRunning, setAutoRunning] = useState(false)
  const [pages, setPages] = useState(3)
  const [delaySec, setDelaySec] = useState(2)

  const logRef = useRef<HTMLDivElement>(null)
  const runningRef = useRef(false)

  // 自动采集运行中：监听 c1688:state 在 stopped/idle 时复位 autoRunning
  useEffect(() => {
    if (state === 'stopped' || state === 'idle') {
      setAutoRunning(false)
      setCollecting(false)
    }
  }, [state])

  // 监听运行态事件
  useEffect(() => {
    const unsubLog = Events.On('c1688:log', (ev: any) => {
      const d = ev?.data
      if (d && (d.msg || d.time)) {
        setLogs(prev => {
          const next = [...prev, { time: d.time || '', msg: d.msg || '' }]
          return next.length > 500 ? next.slice(-500) : next
        })
      }
    })
    const unsubResult = Events.On('c1688:result', (ev: any) => {
      setResults(ev?.data || [])
    })
    const unsubProgress = Events.On('c1688:progress', (ev: any) => {
      void ev
    })
    const unsubState = Events.On('c1688:state', (ev: any) => {
      const s = ev?.data as RunState
      if (s) {
        setState(s)
        if (s === 'stopped' || s === 'idle') {
          runningRef.current = false
        }
      }
    })
    return () => {
      unsubLog?.()
      unsubResult?.()
      unsubProgress?.()
      unsubState?.()
    }
  }, [])

  // 日志自动滚动到底部
  useEffect(() => {
    if (logRef.current) {
      logRef.current.scrollTop = logRef.current.scrollHeight
    }
  }, [logs])

  // ---- 动作 ----

  const handleOpen1688 = async () => {
    try {
      await Col1688Service.OpenLogin()
      setWindowOpen(true)
      success(t('已打开 1688 子窗口首页（在子窗口里手动登录 / 输入关键词 / 选类别）'))
    } catch (e: any) {
      showError(t('打开 1688 失败：') + String(e))
    }
  }

  const handleClose1688 = () => {
    Col1688Service.CloseLogin()
    setWindowOpen(false)
    info(t('已关闭 1688 子窗口'))
  }

  const handleCollect = async () => {
    if (collecting) return
    if (!windowOpen) {
      info(t('请先点「打开 1688」'))
      return
    }
    setCollecting(true)
    try {
      // CollectCurrentPage 返回当前累积的全部结果（已去重）
      const items = await Col1688Service.CollectCurrentPage()
      if (items && items.length > 0) {
        success(t(`采集到 ${items.length} 条（已累积）`))
      } else {
        info(t('未采集到数据；请确认子窗口已加载 1688 列表页且未被拦截'))
      }
    } catch (e: any) {
      showError(t('采集失败：') + String(e))
    } finally {
      setCollecting(false)
    }
  }

  // 自动多页采集：Go 端滚动加载 + 点下一页，共 pages 页，翻页间隔 delaySec。
  const handleAutoCollect = async () => {
    if (!windowOpen) {
      info(t('请先点「打开 1688」并搜到结果页'))
      return
    }
    if (autoRunning) return
    setAutoRunning(true)
    try {
      await Col1688Service.StartPages({ Pages: pages, DelaySec: delaySec, Mode: 'browser', Keyword: '', Category: 'factory', Region: '', MaxCount: 0, HTML: '' })
      success(t('已启动自动采集 ' + pages + ' 页，可在子窗口里手动干预（暂停用「停止」）'))
    } catch (e: any) {
      showError(t('启动自动采集失败：') + String(e))
      setAutoRunning(false)
    }
  }

  const handleStop = async () => {
    try {
      Col1688Service.Stop()
      info(t('已请求停止自动采集'))
    } catch (e: any) {
      showError(t('停止失败：') + String(e))
    }
  }

  const handleDumpDiagnosis = async () => {
    try {
      const path = await Col1688Service.DumpDiagnosis()
      success(t('诊断 HTML 已保存') + '：' + path)
      info(t('请把该文件发给我们分析'))
    } catch (e: any) {
      showError(t('生成诊断失败：') + String(e))
    }
  }

  const handleExport = async () => {
    if (results.length === 0) {
      info(t('暂无数据可导出'))
      return
    }
    try {
      const path = await Col1688Service.ExportCSV()
      setExportPath(path)
      success(t('已导出') + '：' + path)
    } catch (e: any) {
      showError(t('导出失败') + '：' + String(e))
    }
  }

  const copyRow = async (s: Supplier) => {
    const text = [s.name, s.phone, s.address, s.main, s.level, s.url].filter(Boolean).join('\t')
    try {
      await navigator.clipboard.writeText(text)
      success(t('已复制该行'))
    } catch {
      info(t('复制失败'))
    }
  }

  const copyAll = async () => {
    const header = '名称\t电话\t地址\t主营业务\t实力等级\t来源链接\t来源\n'
    const lines = results
      .map(s => [s.name, s.phone, s.address, s.main, s.level, s.url, s.source].join('\t'))
      .join('\n')
    try {
      await navigator.clipboard.writeText(header + lines)
      success(t('已复制全部') + `（${results.length} 行）`)
    } catch {
      info(t('复制失败'))
    }
  }

  // 结果统计
  const stats = useMemo(() => {
    const withPhone = results.filter(s => s.phone && s.phone.trim()).length
    const withAddr = results.filter(s => s.address && s.address.trim()).length
    return { total: results.length, withPhone, withAddr }
  }, [results])

  return (
    <div className="col1688 h-full p-5 text-white/90">
      <div className="max-w-[1500px] mx-auto flex gap-4 h-full min-h-0">
        {/* 左侧：控制 + 结果 */}
        <div className="flex-1 min-w-0 flex flex-col gap-4 overflow-y-auto pr-1">
          {/* 页头 */}
          <div className="flex items-center gap-3">
            <div className="w-11 h-11 rounded-xl bg-gradient-to-br from-[#7C3AED] to-[#A78BFA] flex items-center justify-center flex-shrink-0">
              <Icon name="funnel" size={22} color="white" />
            </div>
            <div>
              <h1 className="text-xl font-bold">{t('1688 工厂供应商采集')}</h1>
              <p className="text-xs text-white/40">{t('在子窗口里手动操作，点「采集当前页」读取数据')}</p>
            </div>
            <div className="ml-auto flex items-center gap-2">
              <StateBadge state={state} />
            </div>
          </div>

          {/* 说明区 */}
          <section className="glass rounded-xl p-4">
            <div className="text-sm font-semibold text-white/70 mb-2">{t('使用步骤')}</div>
            <ol className="text-[11px] leading-5 text-white/45 list-decimal list-inside space-y-1">
              <li>{t('点「打开 1688」到首页登录（如已登录可跳过）')}</li>
              <li>{t('在子窗口里手动输入关键词 + 选类别/地区 + 点供应商卡片/翻页')}</li>
              <li>{t('列表加载好后，点「采集当前页」读取数据（可重复翻页多次采集）')}</li>
              <li>{t('最后点「导出 CSV」保存结果')}</li>
            </ol>
            <p className="text-[11px] text-white/30 mt-3">
              {t('若子窗口被 baxia 弹滑块，请在窗口里手动拖动一次（无 CDP 痕迹，能真正通过）。')}
            </p>
          </section>

          {/* 控制条 */}
          <section className="glass rounded-xl p-4">
            <div className="grid grid-cols-2 md:grid-cols-4 gap-2">
              <CtrlBtn onClick={handleOpen1688} icon="external-link" disabled={collecting || autoRunning} accent>
                {windowOpen ? t('已打开 1688') : t('打开 1688')}
              </CtrlBtn>
              <CtrlBtn onClick={handleClose1688} icon="close" disabled={!windowOpen || collecting || autoRunning}>
                {t('关闭子窗口')}
              </CtrlBtn>
              <CtrlBtn onClick={handleCollect} icon="funnel" disabled={!windowOpen || collecting || autoRunning} accent>
                {collecting ? t('采集中…') : t('采集当前页')}
              </CtrlBtn>
              <CtrlBtn onClick={handleDumpDiagnosis} icon="camera" disabled={!windowOpen || collecting || autoRunning}>
                {t('导出诊断')}
              </CtrlBtn>
              <CtrlBtn onClick={handleExport} icon="download" disabled={results.length === 0}>
                {t('导出 CSV')}
              </CtrlBtn>
              <CtrlBtn onClick={copyAll} icon="copy" disabled={results.length === 0}>
                {t('复制全部')}
              </CtrlBtn>
            </div>

            {/* 自动多页采集 */}
            <div className="mt-3 grid grid-cols-1 sm:grid-cols-4 gap-2 items-center">
              <label className="flex items-center gap-2 text-xs text-white/50">
                {t('页数')}
                <input
                  type="number"
                  min={1}
                  max={20}
                  value={pages}
                  onChange={e => setPages(Math.max(1, Math.min(20, Number(e.target.value) || 1)))}
                  disabled={autoRunning}
                  className="w-14 px-2 py-1.5 rounded-lg bg-white/5 border border-white/10 text-white text-xs focus:outline-none focus:border-[#A78BFA] disabled:opacity-40"
                />
              </label>
              <label className="flex items-center gap-2 text-xs text-white/50">
                {t('翻页间隔(秒)')}
                <input
                  type="number"
                  min={1}
                  max={30}
                  value={delaySec}
                  onChange={e => setDelaySec(Math.max(1, Math.min(30, Number(e.target.value) || 1)))}
                  disabled={autoRunning}
                  className="w-14 px-2 py-1.5 rounded-lg bg-white/5 border border-white/10 text-white text-xs focus:outline-none focus:border-[#A78BFA] disabled:opacity-40"
                />
              </label>
              <CtrlBtn onClick={handleAutoCollect} icon="play" disabled={!windowOpen || autoRunning || collecting} accent className="sm:col-span-2">
                {autoRunning ? t('自动采集中…') : t('自动采集 ' + pages + ' 页')}
              </CtrlBtn>
              <CtrlBtn onClick={handleStop} icon="close" disabled={!autoRunning} className="sm:col-span-1">
                {t('停止')}
              </CtrlBtn>
            </div>
            {exportPath && (
              <div className="text-[11px] text-[#A78BFA] mt-3 truncate">
                {t('最近导出：')}<span className="text-white/50 font-mono">{exportPath}</span>
              </div>
            )}
          </section>

          {/* 结果表 */}
          <section className="glass rounded-xl p-4 flex-1 min-h-[200px] flex flex-col">
            <div className="flex items-center justify-between mb-3">
              <h2 className="text-sm font-semibold text-white/70">
                {t('结果')}
                <span className="ml-2 text-[11px] text-white/30">
                  {stats.total} {t('条')} · {stats.withPhone} {t('有电话')} · {stats.withAddr} {t('有地址')}
                </span>
              </h2>
            </div>

            {results.length === 0 ? (
              <div className="flex-1 flex flex-col items-center justify-center text-center py-10 text-white/30">
                <Icon name="funnel" size={40} color="rgba(167,139,250,0.25)" />
                <p className="mt-3 text-sm">{t('暂无数据，点「打开 1688」后在子窗口里操作，再点「采集当前页」')}</p>
              </div>
            ) : (
              <div className="overflow-auto flex-1 rounded-lg border border-white/10">
                <table className="w-full text-xs">
                  <thead className="sticky top-0 bg-white/5 text-white/50 z-10">
                    <tr>
                      <th className="text-left font-medium px-3 py-2 w-10">#</th>
                      <th className="text-left font-medium px-3 py-2 min-w-[160px]">{t('名称')}</th>
                      <th className="text-left font-medium px-3 py-2 min-w-[110px]">{t('电话')}</th>
                      <th className="text-left font-medium px-3 py-2 min-w-[180px]">{t('地址')}</th>
                      <th className="text-left font-medium px-3 py-2 min-w-[120px]">{t('主营')}</th>
                      <th className="text-left font-medium px-3 py-2 w-20">{t('操作')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {results.map((s, i) => (
                      <tr key={i} className="border-t border-white/5 hover:bg-white/[0.03]">
                        <td className="px-3 py-2 text-white/30 tabular-nums">{i + 1}</td>
                        <td className="px-3 py-2 text-white/90 break-all">{s.name || '—'}</td>
                        <td className="px-3 py-2 text-[#4ADE80] break-all">{s.phone || <span className="text-white/25">{t('未采集到')}</span>}</td>
                        <td className="px-3 py-2 text-white/70 break-all">{s.address || '—'}</td>
                        <td className="px-3 py-2 text-white/50 break-all">{s.main || '—'}</td>
                        <td className="px-3 py-2">
                          <button onClick={() => copyRow(s)} title={t('复制该行')}
                            className="text-white/40 hover:text-[#A78BFA] transition-colors">
                            <Icon name="copy" size={14} />
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </div>

        {/* 右侧：运行日志 */}
        <aside className="w-[300px] flex-shrink-0 glass rounded-xl p-4 hidden lg:flex flex-col min-h-0">
          <div className="flex items-center gap-2 mb-3">
            <Icon name="document" size={15} color="#A78BFA" />
            <h2 className="text-sm font-semibold text-white/70">{t('运行日志')}</h2>
            {logs.length > 0 && (
              <button onClick={() => setLogs([])} className="ml-auto text-[11px] text-white/30 hover:text-white/60">
                {t('清空')}
              </button>
            )}
          </div>
          <div
            ref={logRef}
            className="flex-1 overflow-y-auto bg-black/20 border border-white/10 rounded-lg p-2.5 space-y-1 font-mono text-[11px] leading-relaxed min-h-[120px]"
          >
            {logs.length === 0 ? (
              <p className="text-white/25 text-center py-6">{t('日志将显示在这里')}</p>
            ) : (
              logs.map((l, i) => (
                <div key={i} className="whitespace-pre-wrap break-all">
                  <span className="text-white/30">{l.time}</span>{' '}
                  <span className={l.msg.includes('失败') || l.msg.includes('错误') ? 'text-[#F87171]' : 'text-white/75'}>
                    {l.msg}
                  </span>
                </div>
              ))
            )}
          </div>
        </aside>
      </div>

      <ToastContainer toasts={toasts} onRemove={removeToast} />
    </div>
  )
}

// ============ 子组件 / 局部控件 ============

function StateBadge({ state }: { state: RunState }) {
  const map: Record<RunState, { label: string; cls: string; dot: string }> = {
    idle: { label: '空闲', cls: 'bg-white/5 text-white/40 border-white/10', dot: 'bg-white/30' },
    running: { label: '采集中', cls: 'bg-[#22C55E]/10 text-[#4ADE80] border-[#22C55E]/30', dot: 'bg-[#4ADE80] animate-pulse' },
    paused: { label: '已暂停', cls: 'bg-[#F59E0B]/10 text-[#FBBF24] border-[#F59E0B]/30', dot: 'bg-[#FBBF24]' },
    stopped: { label: '已停止', cls: 'bg-[#EF4444]/10 text-[#F87171] border-[#EF4444]/30', dot: 'bg-[#F87171]' },
  }
  const c = map[state] || map.idle
  return (
    <span className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs border ${c.cls}`}>
      <span className={`w-1.5 h-1.5 rounded-full ${c.dot}`} />
      {t(c.label)}
    </span>
  )
}

function CtrlBtn({ onClick, icon, children, disabled, accent, className }: {
  onClick: () => void; icon: any; children: React.ReactNode; disabled?: boolean; accent?: boolean; className?: string
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      className={`flex items-center justify-center gap-1.5 px-3 py-2 rounded-lg text-sm font-medium transition-all disabled:opacity-30 disabled:cursor-not-allowed ${
        accent
          ? 'bg-gradient-to-r from-[#7C3AED] to-[#A78BFA] text-white hover:opacity-90 shadow-lg shadow-[#7C3AED]/20'
          : 'bg-white/5 text-white/75 hover:bg-white/10 hover:text-white border border-white/10'
      } ${className || ''}`}
    >
      <Icon name={icon} size={15} />
      {children}
    </button>
  )
}

export default Col1688Widget
