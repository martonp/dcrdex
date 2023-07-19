import {
  app,
  PageElement,
  RunOverview
} from './registry'
import { getJSON, postJSON } from './http'
import Doc from './doc'
import BasePage from './basepage'

export default class MarketMakerArchivesPage extends BasePage {
  page: Record<string, PageElement>
  base: number
  quote: number
  host: string

  constructor (main: HTMLElement) {
    super()
    const page = this.page = Doc.idDescendants(main)
    Doc.cleanTemplates(page.runTableRowTmpl)
    this.setup()
    Doc.bind(page.loadRunBtn, 'click', async () => { this.loadButtonClicked() })
    Doc.bind(page.runSelect, 'change', async () => { Doc.hide(page.runTable) })
  }

  async setup () {
    const urlParams = new URLSearchParams(window.location.search)
    const startTimeParam = urlParams.get('startTime')
    const startTime = parseInt(startTimeParam || '0')
    const pastRuns = await this.getPastRuns()
    for (const run of pastRuns) {
      const option = document.createElement('option')
      option.value = `${run}`
      option.textContent = (new Date(run * 1000)).toLocaleString()
      this.page.runSelect.insertBefore(option, this.page.runSelect.firstChild)
      // startTime will be set if we are coming back from th mmlogs or
      // mmsettings page.
      if (startTime === 0 || run === startTime) {
        option.selected = true
      }
    }
    if (startTime > 0) {
      this.loadRun()
    }
  }

  async loadButtonClicked () {
    const runID = parseInt(this.page.runSelect.value || '0')
    const newUrl = `mmarchives?startTime=${runID}`
    app().loadPage(newUrl)
  }

  async loadRun () {
    const page = this.page
    const runID = parseInt(page.runSelect.value || '0')
    const res = await postJSON('/api/runoverview', { runID })
    if (!app().checkResponse(res)) {
      console.error('failed to get run overview', res)
      return
    }
    Doc.empty(page.runTableBody)

    const markets : Record<string, RunOverview> = res.markets
    for (const marketID in markets) {
      const row = page.runTableRowTmpl.cloneNode(true) as HTMLElement
      const rowTmpl = Doc.parseTemplate(row)
      const [host, base, quote] = Doc.parseMarketWithHost(marketID)
      const overview = markets[marketID]
      const stats = overview.stats
      const baseSymbol = app().assets[base].symbol
      const quoteSymbol = app().assets[quote].symbol
      const baseUnitInfo = app().assets[base].unitInfo
      const quoteUnitInfo = app().assets[quote].unitInfo
      const baseLogoPath = Doc.logoPath(baseSymbol)
      const quoteLogoPath = Doc.logoPath(quoteSymbol)

      rowTmpl.host.textContent = host
      rowTmpl.baseMktLogo.src = baseLogoPath
      rowTmpl.quoteMktLogo.src = quoteLogoPath
      rowTmpl.baseSymbol.textContent = baseSymbol
      rowTmpl.quoteSymbol.textContent = quoteSymbol
      rowTmpl.botType.textContent = 'Market Maker'
      rowTmpl.baseChange.textContent = `${Doc.formatCoinValue(stats.baseChange, baseUnitInfo)} ${baseSymbol}`
      rowTmpl.baseChangeLogo.src = baseLogoPath
      rowTmpl.quoteChange.textContent = `${Doc.formatCoinValue(stats.quoteChange, quoteUnitInfo)} ${quoteSymbol}`
      rowTmpl.quoteChangeLogo.src = quoteLogoPath
      rowTmpl.baseFees.textContent = `${Doc.formatCoinValue(stats.baseFees, baseUnitInfo)} ${baseSymbol}`
      rowTmpl.baseFeesLogo.src = baseLogoPath
      rowTmpl.quoteFees.textContent = `${Doc.formatCoinValue(stats.quoteFees, quoteUnitInfo)} ${quoteSymbol}`
      rowTmpl.quoteFeesLogo.src = quoteLogoPath

      let negative = false
      if (stats.fiatGainLoss < 0) {
        negative = true
        stats.fiatGainLoss = -stats.fiatGainLoss
      }
      if (negative) {
        rowTmpl.profit.textContent = `-$${stats.fiatGainLoss.toFixed(2)}`
      } else {
        rowTmpl.profit.textContent = `$${stats.fiatGainLoss.toFixed(2)}`
      }

      Doc.bind(rowTmpl.logsBtn, 'click', () => {
        app().loadPage(`mmlogs?host=${host}&base=${base}&quote=${quote}&startTime=${runID}`)
      })
      Doc.bind(rowTmpl.settings, 'click', () => {
        app().loadPage(`mmsettings?host=${host}&base=${base}&quote=${quote}&startTime=${runID}`)
      })

      page.runTableBody.appendChild(row)
    }
    Doc.show(page.runTable)
  }

  async getPastRuns (): Promise<number[]> {
    const res = await getJSON('/api/archivedmmruns')
    if (!app().checkResponse(res)) {
      console.error('failed to get archived mm runs', res)
      return []
    }
    return res.runs
  }
}
