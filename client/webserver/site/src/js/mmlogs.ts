import {
  app,
  PageElement,
  BotEvent,
  RunStats,
  UnitInfo,
  BotEventNote
} from './registry'
import Doc from './doc'
import { postJSON } from './http'
import BasePage from './basepage'

export default class MarketMakerLogsPage extends BasePage {
  page: Record<string, PageElement>
  base: number
  quote: number
  host: string
  startTime: number // only > 0 for archived logs
  baseUnitInfo: UnitInfo
  quoteUnitInfo: UnitInfo

  constructor (main: HTMLElement) {
    super()
    const page = this.page = Doc.idDescendants(main)
    Doc.cleanTemplates(page.eventTableRowTmpl)
    const urlParams = new URLSearchParams(window.location.search)
    const host = urlParams.get('host')
    const base = urlParams.get('base')
    const quote = urlParams.get('quote')
    const startTimeParam = urlParams.get('startTime')
    const baseID = parseInt(base || '0')
    const quoteID = parseInt(quote || '0')
    const startTime = parseInt(startTimeParam || '0')
    if (!host || !base || !quote) {
      console.log("Missing 'host', 'base', or 'quote' URL parameter")
      return
    }
    this.host = host
    this.base = baseID
    this.quote = quoteID
    this.startTime = startTime

    page.baseHeader.textContent = app().assets[baseID].symbol.toUpperCase()
    page.quoteHeader.textContent = app().assets[quoteID].symbol.toUpperCase()
    page.hostHeader.textContent = host

    const baseLogo = Doc.logoPathFromID(baseID)
    const quoteLogo = Doc.logoPathFromID(quoteID)
    page.baseLogo.src = baseLogo
    page.quoteLogo.src = quoteLogo
    page.baseChangeLogo.src = baseLogo
    page.quoteChangeLogo.src = quoteLogo
    page.baseFeesLogo.src = baseLogo
    page.quoteFeesLogo.src = quoteLogo

    if (startTime > 0) {
      page.runTime.textContent = (new Date(startTime * 1000)).toLocaleString()
      Doc.show(page.runTime)
    }

    this.setup()
  }

  async setup () {
    this.baseUnitInfo = await app().assets[this.base].unitInfo
    this.quoteUnitInfo = await app().assets[this.quote].unitInfo
    const [events, stats] = await this.getRunLogs(this.host, this.base, this.quote, this.startTime)
    this.populateTable(events)
    this.populateStats(stats)
    app().registerNoteFeeder({
      botevent: (note: BotEventNote) => { this.handleBotEventNote(note) }
    })
  }

  handleBotEventNote (note: BotEventNote) {
    if (this.host !== note.host || this.base !== note.base || this.quote !== note.quote) {
      return
    }
    this.newEventRow(note.event)
    this.populateStats(note.stats)
  }

  populateStats (stats: RunStats) {
    const page = this.page
    const baseSymbol = app().assets[this.base].symbol.toUpperCase()
    const quoteSymbol = app().assets[this.quote].symbol.toUpperCase()
    page.baseChange.textContent = `${Doc.formatCoinValue(stats.baseChange, this.baseUnitInfo)} ${baseSymbol}`
    page.quoteChange.textContent = `${Doc.formatCoinValue(stats.quoteChange, this.quoteUnitInfo)} ${quoteSymbol}`
    page.baseFees.textContent = `${Doc.formatCoinValue(stats.baseFees, this.baseUnitInfo)} ${baseSymbol}`
    page.quoteFees.textContent = `${Doc.formatCoinValue(stats.quoteFees, this.quoteUnitInfo)} ${quoteSymbol}`
    let negative = false
    if (stats.fiatGainLoss < 0) {
      negative = true
      stats.fiatGainLoss = -stats.fiatGainLoss
    }
    if (negative) {
      page.profit.textContent = `-$${stats.fiatGainLoss.toFixed(2)}`
    } else {
      page.profit.textContent = `$${stats.fiatGainLoss.toFixed(2)}`
    }
  }

  newEventRow (event: BotEvent) {
    const page = this.page
    const row = page.eventTableRowTmpl.cloneNode(true) as HTMLElement
    const rowTmpl = Doc.parseTemplate(row)
    rowTmpl.eventType.textContent = event.type
    rowTmpl.time.textContent = (new Date(event.timeStamp * 1000)).toLocaleString()

    for (const orderID of event.orderIDs) {
      const a = document.createElement('a')
      a.textContent = `${orderID.substring(0, 8)}...`
      a.href = `/order/${orderID}`
      rowTmpl.orderIDs.appendChild(a)
      rowTmpl.orderIDs.appendChild(document.createElement('br'))
    }

    if (event.matchID && event.matchID !== '') {
      const a = document.createElement('a')
      a.textContent = `${event.matchID.substring(0, 8)}...`
      a.href = `/order/${event.orderIDs[0]}`
      rowTmpl.matchID.appendChild(a)
    }

    if (event.fundingTxID && event.fundingTxID !== '') {
      const a = document.createElement('a')
      a.textContent = `${event.fundingTxID.substring(0, 8)}...`
      rowTmpl.fundingTxID.appendChild(a)
    }

    rowTmpl.baseDelta.textContent = Doc.formatCoinValue(event.baseDelta, this.baseUnitInfo)
    rowTmpl.quoteDelta.textContent = Doc.formatCoinValue(event.quoteDelta, this.quoteUnitInfo)
    rowTmpl.baseFees.textContent = Doc.formatCoinValue(event.baseFees, this.baseUnitInfo)
    rowTmpl.quoteFees.textContent = Doc.formatCoinValue(event.quoteFees, this.quoteUnitInfo)

    page.eventsTableBody.insertBefore(row, page.eventsTableBody.firstChild)
  }

  populateTable (events: BotEvent[]) {
    for (const event of events) {
      this.newEventRow(event)
    }
  }

  async getRunLogs (host: string, baseID: number, quoteID: number, startTime: number) : Promise<[BotEvent[], RunStats]> {
    const req : any = { host, baseID, quoteID }
    if (startTime > 0) req.startTime = startTime
    const res = await postJSON('/api/botlogs', req)
    if (!app().checkResponse(res)) {
      console.error('failed to get bot logs', res)
    }
    return [res.events, res.stats]
  }
}
