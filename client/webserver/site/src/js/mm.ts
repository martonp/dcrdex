import {
  app,
  PageElement,
  Market,
  Exchange,
  BotConfig,
  MMStatusNote,
  BotStatusNote,
  MMStatus,
  BotStatus
} from './registry'
import Doc from './doc'
import BasePage from './basepage'
import { setOptionTemplates } from './opts'
import { bind as bindForm, NewWalletForm } from './forms'

interface HostedMarket extends Market {
  host: string
}

const animationLength = 300

function marketStr (host: string, baseID: number, quoteID: number): string {
  return `${host}-${baseID}-${quoteID}`
}

function parseMarketStr (str: string): [string, number, number] {
  const parts = str.split('-')
  return [parts[0], parseInt(parts[1]), parseInt(parts[2])]
}

export default class MarketMakerPage extends BasePage {
  page: Record<string, PageElement>
  currentForm: HTMLElement
  keyup: (e: KeyboardEvent) => void
  selectHostCallback: (e: Event) => void
  currentNewMarket: HostedMarket
  newWalletForm: NewWalletForm
  mmRunning: boolean

  constructor (main: HTMLElement) {
    super()

    const page = this.page = Doc.idDescendants(main)

    page.forms.querySelectorAll('.form-closer').forEach(el => {
      Doc.bind(el, 'click', () => { this.closePopups() })
    })

    this.keyup = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        this.closePopups()
      }
    }

    this.newWalletForm = new NewWalletForm(
      page.newWalletForm,
      () => { this.addBotSubmit() }
    )

    Doc.bind(page.addBotBtn, 'click', () => { this.showAddBotForm() })
    Doc.bind(page.archivedLogsBtn, 'click', () => { app().loadPage('mmarchives') })
    Doc.bind(page.startBotsBtn, 'click', () => { this.showPWSubmitForm() })
    bindForm(page.pwForm, page.pwSubmit, () => this.startBots())
    Doc.bind(page.stopBotsBtn, 'click', () => { this.stopBots() })
    bindForm(page.addBotForm, page.addBotSubmit, () => { this.addBotSubmit() })

    setOptionTemplates(page)

    Doc.cleanTemplates(page.orderOptTmpl, page.booleanOptTmpl, page.rangeOptTmpl,
      page.assetRowTmpl, page.botTableRowTmpl)

    const selectClicked = async (e: MouseEvent, isBase: boolean): Promise<void> => {
      e.stopPropagation()
      console.log('select clicked')
      const select = isBase ? page.baseSelect : page.quoteSelect
      const m = Doc.descendentMetrics(page.addBotForm, select)
      page.assetDropdown.style.left = `${m.bodyLeft}px`
      page.assetDropdown.style.top = `${m.bodyTop}px`

      const counterAsset = isBase ? this.currentNewMarket.quoteid : this.currentNewMarket.baseid
      const clickedSymbol = isBase ? this.currentNewMarket.basesymbol : this.currentNewMarket.quotesymbol

      // Look through markets for other base assets for the counter asset.
      const matches: Set<string> = new Set()
      const otherAssets: Set<string> = new Set()

      for (const mkt of await this.sortedMarkets()) {
        otherAssets.add(mkt.basesymbol)
        otherAssets.add(mkt.quotesymbol)
        const [firstID, secondID] = isBase ? [mkt.quoteid, mkt.baseid] : [mkt.baseid, mkt.quoteid]
        const [firstSymbol, secondSymbol] = isBase ? [mkt.quotesymbol, mkt.basesymbol] : [mkt.basesymbol, mkt.quotesymbol]
        if (firstID === counterAsset) matches.add(secondSymbol)
        else if (secondID === counterAsset) matches.add(firstSymbol)
      }
      const options = Array.from(matches)
      options.sort((a: string, b: string) => a.localeCompare(b))
      for (const symbol of options) otherAssets.delete(symbol)
      const nonOptions = Array.from(otherAssets)
      nonOptions.sort((a: string, b: string) => a.localeCompare(b))

      Doc.empty(page.assetDropdown)
      const addOptions = (symbols: string[], avail: boolean): void => {
        for (const symbol of symbols) {
          const row = this.assetRow(symbol)
          Doc.bind(row, 'click', (e: MouseEvent) => {
            e.stopPropagation()
            if (symbol === clickedSymbol) return this.hideAssetDropdown() // no change
            // this.leaveEditMode()
            if (isBase) this.setCreationBase(symbol)
            else this.setCreationQuote(symbol)
          })
          if (!avail) row.classList.add('ghost')
          page.assetDropdown.appendChild(row)
        }
      }
      addOptions(options, true)
      addOptions(nonOptions, false)
      Doc.show(page.assetDropdown)
      const clicker = (e: MouseEvent): void => {
        if (Doc.mouseInElement(e, page.assetDropdown)) return
        this.hideAssetDropdown()
        Doc.unbind(document, 'click', clicker)
      }
      Doc.bind(document, 'click', clicker)
    }

    Doc.bind(page.baseSelect, 'click', (e: MouseEvent) => selectClicked(e, true))
    Doc.bind(page.quoteSelect, 'click', (e: MouseEvent) => selectClicked(e, false))

    this.setup()
  }

  async setup () {
    const page = this.page

    const status = await app().getMarketMakingStatus()
    console.log(status)
    this.mmRunning = status.running
    const botConfigs = await app().getAllMarketMakingConfig()
    app().registerNoteFeeder({
      mmstatus: (note: MMStatusNote) => { this.handleMMStatusNote(note) },
      botstatus: (note: BotStatusNote) => { this.handleBotStatusNote(note) }
    })

    const noBots = !botConfigs || botConfigs.length === 0
    Doc.setVis(noBots, page.noBotsHeader)
    Doc.setVis(!noBots, page.botTable)
    if (noBots) return

    if (this.mmRunning) {
      page.runningSinceTime.textContent = (new Date(status.runStart * 1000)).toLocaleString()
    }
    Doc.setVis(this.mmRunning, page.stopBotsBtn, page.runningSinceMsg)
    Doc.setVis(!this.mmRunning, page.startBotsBtn, page.addBotBtn)
    this.setupBotTable(botConfigs, status)
  }

  handleMMStatusNote (note: MMStatusNote) {
    const page = this.page
    this.mmRunning = note.running

    if (this.mmRunning) {
      page.runningSinceTime.textContent = (new Date(note.runStart * 1000)).toLocaleString()
    }
    Doc.setVis(note.running, page.stopBotsBtn, page.runningHeader, page.profitHeader, page.logsHeader, page.runningSinceMsg)
    Doc.setVis(!note.running, page.startBotsBtn, page.addBotBtn, page.enabledHeader, page.removeHeader)

    const tableRows = page.botTableBody.children
    for (let i = 0; i < tableRows.length; i++) {
      const row = tableRows[i] as PageElement
      if (!this.mmRunning) this.updateTableRow(row, undefined)
      else {
        if (!note.bots || note.bots.length === 0) return
        const status = note.bots.find((s: BotStatus) => {
          const rowID = marketStr(s.host, s.base, s.quote)
          return row.id === rowID
        })
        this.updateTableRow(row, status)
      }
    }
  }

  async handleBotStatusNote (note: BotStatusNote) {
    const page = this.page
    const tableRows = page.botTableBody.children
    const status = note.status
    const rowID = marketStr(status.host, status.base, status.quote)
    for (let i = 0; i < tableRows.length; i++) {
      const row = tableRows[i] as PageElement
      if (row.id === rowID) {
        this.updateTableRow(row, status)
        return
      }
    }
  }

  async updateTableRow (row: PageElement, status: BotStatus | undefined) {
    const [host, base, quote] = parseMarketStr(row.id)
    const cfg = await app().getMarketMakingConfig(host, base, quote)
    if (!cfg) {
      console.error('no config for', host, base, quote)
      return
    }
    const rowTmpl = Doc.parseTemplate(row)

    Doc.setVis(this.mmRunning, rowTmpl.running, rowTmpl.runningBaseBalanceTd, rowTmpl.runningQuoteBalanceTd, rowTmpl.profitTd, rowTmpl.logsTd)
    Doc.setVis(!this.mmRunning, rowTmpl.enabled, rowTmpl.stoppedBaseBalanceTd, rowTmpl.stoppedQuoteBalanceTd, rowTmpl.removeTd)

    Doc.setVis(status && status.running, rowTmpl.runningIcon)
    Doc.setVis(!status || !status.running, rowTmpl.notRunningIcon)

    if (this.mmRunning) {
      Doc.setVis(status && status.running,
        rowTmpl.runningBaseBalance, rowTmpl.runningBaseBalanceLogo, rowTmpl.baseBalanceInfo,
        rowTmpl.runningQuoteBalance, rowTmpl.runningQuoteBalanceLogo, rowTmpl.quoteBalanceInfo,
        rowTmpl.profit, rowTmpl.profitInfo, rowTmpl.logsBtn)
    }

    if (!status || !status.running) return

    const baseAssetUnitInfo = app().assets[cfg.baseAsset].unitInfo
    const quoteAssetUnitInfo = app().assets[cfg.quoteAsset].unitInfo

    const baseBalance = status.baseBalance
    const totalBaseBalance = baseBalance.available + baseBalance.fundingOrder + baseBalance.pendingRedeem + baseBalance.pendingRefund
    const totalBaseBalanceStr = Doc.formatCoinValue(totalBaseBalance, baseAssetUnitInfo)
    const availableBaseBalanceStr = Doc.formatCoinValue(baseBalance.available, baseAssetUnitInfo)
    const fundingBaseBalanceStr = Doc.formatCoinValue(baseBalance.fundingOrder, baseAssetUnitInfo)
    const pendingRedeemBaseBalanceStr = Doc.formatCoinValue(baseBalance.pendingRedeem, baseAssetUnitInfo)
    const pendingRefundBaseBalanceStr = Doc.formatCoinValue(baseBalance.pendingRefund, baseAssetUnitInfo)
    rowTmpl.runningBaseBalance.textContent = totalBaseBalanceStr
    rowTmpl.runningBaseBalanceTotal.textContent = totalBaseBalanceStr
    rowTmpl.runningBaseBalanceAvailable.textContent = availableBaseBalanceStr
    rowTmpl.runningBaseBalanceFunding.textContent = fundingBaseBalanceStr
    rowTmpl.runningBaseBalancePendingRedeem.textContent = pendingRedeemBaseBalanceStr
    rowTmpl.runningBaseBalancePendingRefund.textContent = pendingRefundBaseBalanceStr

    const quoteBalance = status.quoteBalance
    const totalQuoteBalance = quoteBalance.available + quoteBalance.fundingOrder + quoteBalance.pendingRedeem + quoteBalance.pendingRefund
    const totalQuoteBalanceStr = Doc.formatCoinValue(totalQuoteBalance, quoteAssetUnitInfo)
    const availableQuoteBalanceStr = Doc.formatCoinValue(quoteBalance.available, quoteAssetUnitInfo)
    const fundingQuoteBalanceStr = Doc.formatCoinValue(quoteBalance.fundingOrder, quoteAssetUnitInfo)
    const pendingRedeemQuoteBalanceStr = Doc.formatCoinValue(quoteBalance.pendingRedeem, quoteAssetUnitInfo)
    const pendingRefundQuoteBalanceStr = Doc.formatCoinValue(quoteBalance.pendingRefund, quoteAssetUnitInfo)
    rowTmpl.runningQuoteBalance.textContent = totalQuoteBalanceStr
    rowTmpl.runningQuoteBalanceTotal.textContent = totalQuoteBalanceStr
    rowTmpl.runningQuoteBalanceAvailable.textContent = availableQuoteBalanceStr
    rowTmpl.runningQuoteBalanceFunding.textContent = fundingQuoteBalanceStr
    rowTmpl.runningQuoteBalancePendingRedeem.textContent = pendingRedeemQuoteBalanceStr
    rowTmpl.runningQuoteBalancePendingRefund.textContent = pendingRefundQuoteBalanceStr

    rowTmpl.profit.textContent = `$${Doc.formatFiatValue(status.fiatGainLoss)}`
    if (status.fiatGainLoss < 0) {
      rowTmpl.profit.classList.add('loss-color')
      rowTmpl.profit.classList.remove('profit-color')
    } else {
      rowTmpl.profit.classList.remove('loss-color')
      rowTmpl.profit.classList.add('profit-color')
    }

    let baseChangeStr = Doc.formatCoinValue(status.baseChange, baseAssetUnitInfo)
    if (status.baseChange > 0) {
      baseChangeStr = `+${baseChangeStr}`
    }
    let quoteChangeStr = Doc.formatCoinValue(status.quoteChange, quoteAssetUnitInfo)
    if (status.quoteChange > 0) {
      quoteChangeStr = `+${quoteChangeStr}`
    }
    const baseFeesStr = Doc.formatCoinValue(status.baseFees, baseAssetUnitInfo)
    const quoteFeesStr = Doc.formatCoinValue(status.quoteFees, quoteAssetUnitInfo)
    rowTmpl.baseChange.textContent = baseChangeStr
    if (status.baseChange < 0) {
      rowTmpl.baseChange.classList.add('loss-color')
      rowTmpl.baseChange.classList.remove('profit-color')
    } else {
      rowTmpl.baseChange.classList.remove('loss-color')
      rowTmpl.baseChange.classList.add('profit-color')
    }
    rowTmpl.quoteChange.textContent = quoteChangeStr
    if (status.quoteChange < 0) {
      rowTmpl.quoteChange.classList.add('loss-color')
      rowTmpl.quoteChange.classList.remove('profit-color')
    } else {
      rowTmpl.quoteChange.classList.remove('loss-color')
      rowTmpl.quoteChange.classList.add('profit-color')
    }
    rowTmpl.baseFees.textContent = baseFeesStr
    rowTmpl.quoteFees.textContent = quoteFeesStr
  }

  setupBotTable (botConfigs: BotConfig[], mmStatus: MMStatus) {
    const page = this.page
    Doc.empty(page.botTableBody)

    Doc.setVis(this.mmRunning, page.runningHeader, page.profitHeader, page.logsHeader)
    Doc.setVis(!this.mmRunning, page.enabledHeader, page.removeHeader)

    for (const botCfg of botConfigs) {
      const row = page.botTableRowTmpl.cloneNode(true) as PageElement
      row.id = marketStr(botCfg.host, botCfg.baseAsset, botCfg.quoteAsset)
      const rowTmpl = Doc.parseTemplate(row)

      const baseSymbol = app().assets[botCfg.baseAsset].symbol
      const quoteSymbol = app().assets[botCfg.quoteAsset].symbol
      const baseLogoPath = Doc.logoPath(baseSymbol)
      const quoteLogoPath = Doc.logoPath(quoteSymbol)

      rowTmpl.enabledCheckbox.checked = !botCfg.disabled
      rowTmpl.enabledCheckbox.onclick = async () => {
        app().setMarketMakingEnabled(botCfg.host, botCfg.baseAsset, botCfg.quoteAsset, !!rowTmpl.enabledCheckbox.checked)
      }
      rowTmpl.host.textContent = botCfg.host
      rowTmpl.baseMktLogo.src = baseLogoPath
      rowTmpl.quoteMktLogo.src = quoteLogoPath
      rowTmpl.baseSymbol.textContent = baseSymbol.toUpperCase()
      rowTmpl.quoteSymbol.textContent = quoteSymbol.toUpperCase()
      rowTmpl.botType.textContent = 'Market Maker'
      rowTmpl.baseBalance.textContent = this.walletBalanceStr(botCfg.baseAsset, botCfg.baseBalance)
      rowTmpl.quoteBalance.textContent = this.walletBalanceStr(botCfg.quoteAsset, botCfg.quoteBalance)
      rowTmpl.baseBalanceLogo.src = baseLogoPath
      rowTmpl.quoteBalanceLogo.src = quoteLogoPath
      rowTmpl.runningBaseBalanceLogo.src = baseLogoPath
      rowTmpl.runningQuoteBalanceLogo.src = quoteLogoPath
      rowTmpl.baseChangeLogo.src = baseLogoPath
      rowTmpl.quoteChangeLogo.src = quoteLogoPath
      rowTmpl.baseFeesLogo.src = baseLogoPath
      rowTmpl.quoteFeesLogo.src = quoteLogoPath
      rowTmpl.remove.onclick = async () => {
        await app().removeMarketMakingConfig(botCfg)
        row.remove()
        const mmCfg = await app().getAllMarketMakingConfig()
        const noBots = !mmCfg || !mmCfg.length
        Doc.setVis(noBots, page.noBotsHeader)
        Doc.setVis(!noBots, page.botTable)
      }
      rowTmpl.settings.onclick = () => {
        app().loadPage(`mmsettings?host=${botCfg.host}&base=${botCfg.baseAsset}&quote=${botCfg.quoteAsset}`)
      }
      page.botTableBody.appendChild(row)
      let botStatus: BotStatus | undefined
      console.log(mmStatus.bots)
      if (mmStatus.bots) {
        botStatus = mmStatus.bots.find((s: BotStatus) => {
          const rowID = marketStr(s.host, s.base, s.quote)
          return row.id === rowID
        })
        console.log('found bot status = ', botStatus)
      }

      Doc.bind(rowTmpl.baseBalanceInfo, 'mouseover', () => {
        Doc.show(rowTmpl.baseBalanceHoverContainer)
      })
      Doc.bind(rowTmpl.baseBalanceInfo, 'mouseout', () => {
        Doc.hide(rowTmpl.baseBalanceHoverContainer)
      })
      Doc.bind(rowTmpl.quoteBalanceInfo, 'mouseover', () => {
        Doc.show(rowTmpl.quoteBalanceHoverContainer)
      })
      Doc.bind(rowTmpl.quoteBalanceInfo, 'mouseout', () => {
        Doc.hide(rowTmpl.quoteBalanceHoverContainer)
      })
      Doc.bind(rowTmpl.profitInfo, 'mouseover', () => {
        Doc.show(rowTmpl.profitHoverContainer)
      })
      Doc.bind(rowTmpl.profitInfo, 'mouseout', () => {
        Doc.hide(rowTmpl.profitHoverContainer)
      })
      Doc.bind(rowTmpl.logsBtn, 'click', () => {
        app().loadPage(`mmlogs?host=${botCfg.host}&base=${botCfg.baseAsset}&quote=${botCfg.quoteAsset}`)
      })
      this.updateTableRow(row, botStatus)
    }
  }

  unload (): void {
    Doc.unbind(document, 'keyup', this.keyup)
  }

  assetRow (symbol: string): PageElement {
    const row = this.page.assetRowTmpl.cloneNode(true) as PageElement
    const tmpl = Doc.parseTemplate(row)
    tmpl.logo.src = Doc.logoPath(symbol)
    Doc.empty(tmpl.symbol)
    tmpl.symbol.appendChild(Doc.symbolize(symbol))
    return row
  }

  hideAssetDropdown (): void {
    const page = this.page
    page.assetDropdown.scrollTop = 0
    Doc.hide(page.assetDropdown)
  }

  async setMarket (mkts: HostedMarket[]): Promise<void> {
    const page = this.page

    const mkt = mkts[0]
    this.currentNewMarket = mkt

    Doc.empty(page.baseSelect, page.quoteSelect)
    page.baseSelect.appendChild(this.assetRow(mkt.basesymbol))
    page.quoteSelect.appendChild(this.assetRow(mkt.quotesymbol))
    this.hideAssetDropdown()

    const addMarketSelect = (mkt: HostedMarket, el: PageElement) => {
      Doc.setContent(el,
        Doc.symbolize(mkt.basesymbol),
        new Text('-') as any,
        Doc.symbolize(mkt.quotesymbol),
        new Text(' @ ') as any,
        new Text(mkt.host) as any
      )
    }

    Doc.hide(page.marketSelect, page.marketOneChoice)
    if (mkts.length === 1) {
      Doc.show(page.marketOneChoice)
      addMarketSelect(mkt, page.marketOneChoice)
    } else {
      Doc.show(page.marketSelect)
      Doc.empty(page.marketSelect)
      for (const mkt of mkts) {
        const opt = document.createElement('option')
        page.marketSelect.appendChild(opt)
        opt.value = `${mkt.host} ${mkt.name}`
        addMarketSelect(mkt, opt)
      }
    }
  }

  async setCreationBase (symbol: string) {
    const counterAsset = this.currentNewMarket.quotesymbol
    const markets = await this.sortedMarkets()
    const options: HostedMarket[] = []
    // Best option: find an exact match.
    for (const mkt of markets) if (mkt.basesymbol === symbol && mkt.quotesymbol === counterAsset) options.push(mkt)
    // Next best option: same assets, reversed order.
    for (const mkt of markets) if (mkt.quotesymbol === symbol && mkt.basesymbol === counterAsset) options.push(mkt)
    // If we have exact matches, we're done.
    if (options.length > 0) return this.setMarket(options)
    // No exact matches. Must have selected a ghost-class market. Next best
    // option will be the first market where the selected asset is a base asset.
    for (const mkt of markets) if (mkt.basesymbol === symbol) return this.setMarket([mkt])
    // Last option: Market where this is the quote asset.
    for (const mkt of markets) if (mkt.quotesymbol === symbol) return this.setMarket([mkt])
  }

  async setCreationQuote (symbol: string) {
    const counterAsset = this.currentNewMarket.basesymbol
    const markets = await this.sortedMarkets()
    const options: HostedMarket[] = []
    for (const mkt of markets) if (mkt.quotesymbol === symbol && mkt.basesymbol === counterAsset) options.push(mkt)
    for (const mkt of markets) if (mkt.basesymbol === symbol && mkt.quotesymbol === counterAsset) options.push(mkt)
    if (options.length > 0) return this.setMarket(options)
    for (const mkt of markets) if (mkt.quotesymbol === symbol) return this.setMarket([mkt])
    for (const mkt of markets) if (mkt.basesymbol === symbol) return this.setMarket([mkt])
  }

  walletBalanceStr (assetID: number, percentage: number): string {
    const asset = app().assets[assetID]
    const wallet = asset.wallet
    const balance = wallet.balance.available
    const unitInfo = asset.unitInfo
    const assetValue = Doc.formatCoinValue((balance * percentage) / 100, unitInfo)
    return `${percentage}% (${assetValue} ${asset.symbol.toUpperCase()})`
  }

  // TODO: HANDLE MULTIPLE SERVERS
  async showAddBotForm (noAnimation?: boolean) {
    this.setMarket([(await this.sortedMarkets())[0]])
    this.showForm(this.page.addBotForm, noAnimation)
  }

  async showPWSubmitForm () {
    const page = this.page
    this.showForm(page.pwForm)
  }

  async startBots () {
    const page = this.page
    try {
      const pw = page.pwInput.value
      this.page.pwInput.value = ''
      this.closePopups()
      await app().startMarketMaking(pw || '')
    } catch (e) {
      page.mmErr.textContent = e.message
      Doc.show(page.mmErr)
      setTimeout(() => { Doc.hide(page.mmErr) }, 3000)
    }
  }

  async stopBots () {
    await app().stopMarketMaking()
  }

  addBotSubmit () {
    const currMkt = this.currentNewMarket
    if (!app().walletMap[currMkt.baseid]) {
      this.newWalletForm.setAsset(currMkt.baseid)
      this.showForm(this.page.newWalletForm)
      return
    }
    if (!app().walletMap[currMkt.quoteid]) {
      this.newWalletForm.setAsset(currMkt.quoteid)
      this.showForm(this.page.newWalletForm)
      return
    }

    app().loadPage(`mmsettings?host=${currMkt.host}&base=${currMkt.baseid}&&quote=${currMkt.quoteid}`)
  }

  /* showForm shows a modal form with a little animation. */
  async showForm (form: HTMLElement, noAnimation?: boolean): Promise<void> {
    this.currentForm = form
    const page = this.page
    Doc.hide(page.pwForm, page.newWalletForm, page.addBotForm)
    if (!noAnimation) {
      form.style.right = '10000px'
    }
    Doc.show(page.forms, form)
    if (!noAnimation) {
      const shift = (page.forms.offsetWidth + form.offsetWidth) / 2
      await Doc.animate(animationLength, progress => {
        form.style.right = `${(1 - progress) * shift}px`
      }, 'easeOutHard')
      form.style.right = '0'
    }
  }

  closePopups (): void {
    Doc.hide(this.page.forms)
  }

  async sortedMarkets (): Promise<HostedMarket[]> {
    const mkts: HostedMarket[] = []
    const convertMarkets = (xc: Exchange): HostedMarket[] => {
      if (!xc.markets) return []
      return Object.values(xc.markets).map((mkt: Market) => Object.assign({ host: xc.host }, mkt))
    }
    for (const xc of Object.values(app().user.exchanges)) mkts.push(...convertMarkets(xc))

    const mmCfg = await app().getAllMarketMakingConfig()
    const existingMarkets : Record<string, boolean> = {}
    for (const cfg of mmCfg) {
      existingMarkets[marketStr(cfg.host, cfg.baseAsset, cfg.quoteAsset)] = true
    }
    const filteredMkts = mkts.filter((mkt) => {
      return !existingMarkets[marketStr(mkt.host, mkt.baseid, mkt.quoteid)]
    })
    filteredMkts.sort((a: Market, b: Market) => {
      if (!a.spot) {
        if (!b.spot) return a.name.localeCompare(b.name)
        return -1
      }
      if (!b.spot) return 1
      // Sort by lots.
      return b.spot.vol24 / b.lotsize - a.spot.vol24 / a.lotsize
    })
    return filteredMkts
  }
}
