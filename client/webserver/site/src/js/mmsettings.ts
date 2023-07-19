import {
  PageElement,
  BotConfig,
  XYRange,
  OrderPlacement,
  BalanceType,
  app,
  MarketReport
} from './registry'
import { postJSON } from './http'
import Doc from './doc'
import BasePage from './basepage'
import { setOptionTemplates, XYRangeHandler } from './opts'

const GapStrategyMultiplier = 'multiplier'
const GapStrategyAbsolute = 'absolute'
const GapStrategyAbsolutePlus = 'absolute-plus'
const GapStrategyPercent = 'percent'
const GapStrategyPercentPlus = 'percent-plus'

const driftToleranceRange: XYRange = {
  start: {
    label: '0%',
    x: 0,
    y: 0
  },
  end: {
    label: '1%',
    x: 0.01,
    y: 1
  },
  xUnit: '',
  yUnit: '%'
}

const oracleBiasRange: XYRange = {
  start: {
    label: '-1%',
    x: -0.01,
    y: -1
  },
  end: {
    label: '1%',
    x: 0.01,
    y: 1
  },
  xUnit: '',
  yUnit: '%'
}

const oracleWeightRange: XYRange = {
  start: {
    label: '0%',
    x: 0,
    y: 0
  },
  end: {
    label: '100%',
    x: 1,
    y: 100
  },
  xUnit: '',
  yUnit: '%'
}

const splitTxBufferRange: XYRange = {
  start: {
    label: '0%',
    x: 0,
    y: 0
  },
  end: {
    label: '100%',
    x: 100,
    y: 100
  },
  xUnit: '%',
  yUnit: '%'
}

const defaultMarketMakingConfig = {
  gapStrategy: GapStrategyMultiplier,
  sellPlacements: [],
  buyPlacements: [],
  driftTolerance: 0.001,
  oracleWeighting: 0.1,
  oracleBias: 0,
  splitTxAllowed: true,
  splitBuffer: 5
}

export default class MarketMakerSettingsPage extends BasePage {
  page: Record<string, PageElement>
  currentMarket: string
  originalConfig: BotConfig
  updatedConfig: BotConfig
  creatingNewBot: boolean
  host: string
  baseID: number
  quoteID: number
  oracleBiasRangeHandler: XYRangeHandler
  oracleWeightingRangeHandler: XYRangeHandler
  splitBufferRangeHandler: XYRangeHandler
  driftToleranceRangeHandler: XYRangeHandler

  constructor (main: HTMLElement) {
    super()

    const page = (this.page = Doc.idDescendants(main))

    setOptionTemplates(page)
    Doc.cleanTemplates(
      page.orderOptTmpl,
      page.booleanOptTmpl,
      page.rangeOptTmpl,
      page.placementRowTmpl,
      page.oracleTmpl
    )

    Doc.bind(page.resetButton, 'click', () => { this.setOriginalValues(false) })
    Doc.bind(page.updateButton, 'click', () => {
      this.saveSettings()
      Doc.show(page.settingsUpdatedMsg)
      setTimeout(() => {
        Doc.hide(page.settingsUpdatedMsg)
      }, 2000)
    })
    Doc.bind(page.createButton, 'click', async () => {
      await this.saveSettings()
      app().loadPage('mm')
    })
    Doc.bind(page.cancelButton, 'click', () => {
      app().loadPage('mm')
    })

    const urlParams = new URLSearchParams(window.location.search)
    const host = urlParams.get('host')
    const base = urlParams.get('base')
    const quote = urlParams.get('quote')
    if (!host || !base || !quote) {
      console.log("Missing 'host', 'base', or 'quote' URL parameter")
      return
    }
    this.baseID = parseInt(base)
    this.quoteID = parseInt(quote)
    this.host = host
    page.baseHeader.textContent = app().assets[this.baseID].symbol.toUpperCase()
    page.quoteHeader.textContent = app().assets[this.quoteID].symbol.toUpperCase()
    page.hostHeader.textContent = host

    page.baseBalanceLogo.src = Doc.logoPathFromID(this.baseID)
    page.quoteBalanceLogo.src = Doc.logoPathFromID(this.quoteID)
    page.baseLogo.src = Doc.logoPathFromID(this.baseID)
    page.quoteLogo.src = Doc.logoPathFromID(this.quoteID)

    this.setup()
  }

  async setup () {
    const page = this.page
    const botConfigs = await app().getMarketMakingConfig()
    const status = await app().getMarketMakingStatus()

    for (const cfg of botConfigs) {
      if (cfg.host === this.host && cfg.baseAsset === this.baseID && cfg.quoteAsset === this.quoteID) {
        this.originalConfig = JSON.parse(JSON.stringify(cfg))
        this.updatedConfig = JSON.parse(JSON.stringify(cfg))
        break
      }
    }

    if (!this.updatedConfig) {
      this.originalConfig = JSON.parse(JSON.stringify({
        host: this.host,
        baseAsset: this.baseID,
        quoteAsset: this.quoteID,
        baseBalanceType: BalanceType.Percentage,
        baseBalance: 0,
        quoteBalanceType: BalanceType.Percentage,
        quoteBalance: 0,
        marketMakingConfig: defaultMarketMakingConfig,
        disabled: false
      }))
      this.updatedConfig = JSON.parse(JSON.stringify(this.originalConfig))
      this.creatingNewBot = true

      Doc.hide(page.updateButton, page.resetButton)
      Doc.show(page.createButton)
    }

    if (status.running) {
      Doc.hide(page.updateButton, page.createButton, page.resetButton)
    }

    this.setupSettings(status.running)
    this.setOriginalValues(status.running)
    this.setupBalanceSelectors(botConfigs, status.running)
    Doc.show(page.botSettingsContainer)

    this.fetchOracles()
  }

  updateModifiedMarkers () {
    if (this.creatingNewBot) return
    const page = this.page
    const originalMMCfg = this.originalConfig.marketMakingConfig
    const updatedMMCfg = this.updatedConfig.marketMakingConfig
    const gapStrategyModified = originalMMCfg.gapStrategy !== updatedMMCfg.gapStrategy
    page.gapStrategySelect.classList.toggle('modified', gapStrategyModified)
    let buyPlacementsModified = false
    if (originalMMCfg.buyPlacements.length !== updatedMMCfg.buyPlacements.length) {
      buyPlacementsModified = true
    } else {
      for (let i = 0; i < originalMMCfg.buyPlacements.length; i++) {
        if (originalMMCfg.buyPlacements[i].lots !== updatedMMCfg.buyPlacements[i].lots ||
            originalMMCfg.buyPlacements[i].gapFactor !== updatedMMCfg.buyPlacements[i].gapFactor) {
          buyPlacementsModified = true
          break
        }
      }
    }

    page.buyPlacementsTableWrapper.classList.toggle('modified', buyPlacementsModified)

    let sellPlacementsModified = false
    if (originalMMCfg.sellPlacements.length !== updatedMMCfg.sellPlacements.length) {
      sellPlacementsModified = true
    } else {
      for (let i = 0; i < originalMMCfg.sellPlacements.length; i++) {
        if (originalMMCfg.sellPlacements[i].lots !== updatedMMCfg.sellPlacements[i].lots ||
            originalMMCfg.sellPlacements[i].gapFactor !== updatedMMCfg.sellPlacements[i].gapFactor) {
          sellPlacementsModified = true
          break
        }
      }
    }
    page.sellPlacementsTableWrapper.classList.toggle('modified', sellPlacementsModified)

    const driftToleranceModified = originalMMCfg.driftTolerance !== updatedMMCfg.driftTolerance
    page.driftToleranceContainer.classList.toggle('modified', driftToleranceModified)

    const oracleBiasModified = originalMMCfg.oracleBias !== updatedMMCfg.oracleBias
    page.oracleBiasContainer.classList.toggle('modified', oracleBiasModified)

    const oracleWeightingModified = originalMMCfg.oracleWeighting !== updatedMMCfg.oracleWeighting
    page.oracleWeightingContainer.classList.toggle('modified', oracleWeightingModified)

    const emptyMarketRateModified = originalMMCfg.emptyMarketRate !== updatedMMCfg.emptyMarketRate
    page.emptyMarketRateInput.classList.toggle('modified', emptyMarketRateModified)
    page.emptyMarketRateCheckbox.classList.toggle('modified', emptyMarketRateModified && !page.emptyMarketRateCheckbox.checked)

    const splitBufferAllowedModified = originalMMCfg.splitTxAllowed !== updatedMMCfg.splitTxAllowed
    page.splitTxCheckbox.classList.toggle('modified', splitBufferAllowedModified)

    const splitBufferModified = originalMMCfg.splitBuffer !== updatedMMCfg.splitBuffer
    page.splitBufferContainer.classList.toggle('modified', splitBufferModified)

    const baseBalanceModified = this.originalConfig.baseBalance !== this.updatedConfig.baseBalance
    page.baseBalanceContainer.classList.toggle('modified', baseBalanceModified)

    const quoteBalanceModified = this.originalConfig.quoteBalance !== this.updatedConfig.quoteBalance
    page.quoteBalanceContainer.classList.toggle('modified', quoteBalanceModified)
  }

  gapFactorHeaderUnitRange (gapFactor: string) : [string, string] {
    switch (gapFactor) {
      case GapStrategyMultiplier:
        return ['Multiplier', 'x']
      case GapStrategyAbsolute:
      case GapStrategyAbsolutePlus: {
        const rateUnit = `${app().assets[this.quoteID].symbol}/${app().assets[this.baseID].symbol}`
        return ['Rate', rateUnit]
      }
      case GapStrategyPercent:
      case GapStrategyPercentPlus:
        return ['Percent', '%']
      default:
        throw new Error(`Unknown gap factor ${gapFactor}`)
    }
  }

  checkGapFactorRange (gapFactor: string, value: number) : (string | null) {
    switch (gapFactor) {
      case GapStrategyMultiplier:
        if (value < 1 || value > 100) {
          return 'Multiplier must be between 1 and 100'
        }
        return null
      case GapStrategyAbsolute:
      case GapStrategyAbsolutePlus:
        if (value <= 0) {
          return 'Rate must be greater than 0'
        }
        return null
      case GapStrategyPercent:
      case GapStrategyPercentPlus:
        if (value <= 0 || value > 10) {
          return 'Percent must be between 0 and 10'
        }
        return null
      default: {
        throw new Error(`Unknown gap factor ${gapFactor}`)
      }
    }
  }

  convertGapFactor (gapFactor: number, gapStrategy: string, toDisplay: boolean): number {
    switch (gapStrategy) {
      case GapStrategyMultiplier:
      case GapStrategyAbsolute:
      case GapStrategyAbsolutePlus:
        return gapFactor
      case GapStrategyPercent:
      case GapStrategyPercentPlus:
        if (toDisplay) {
          return gapFactor * 100
        }
        return gapFactor / 100
      default:
        throw new Error(`Unknown gap factor ${gapStrategy}`)
    }
  }

  addPlacement (isBuy: boolean, initialLoadPlacement: OrderPlacement | null, running: boolean) {
    const page = this.page

    let tableBody: PageElement = page.sellPlacementsTableBody
    let addPlacementRow: PageElement = page.addSellPlacementRow
    let lotsElement: PageElement = page.addSellPlacementLots
    let gapFactorElement: PageElement = page.addSellPlacementGapFactor
    let errElement: PageElement = page.sellPlacementsErr
    if (isBuy) {
      tableBody = page.buyPlacementsTableBody
      addPlacementRow = page.addBuyPlacementRow
      lotsElement = page.addBuyPlacementLots
      gapFactorElement = page.addBuyPlacementGapFactor
      errElement = page.buyPlacementsErr
    }

    const getPlacementsList = (buy: boolean) : OrderPlacement[] => {
      if (buy) {
        return this.updatedConfig.marketMakingConfig.buyPlacements
      }
      return this.updatedConfig.marketMakingConfig.sellPlacements
    }

    const updateArrowVis = () => {
      for (let i = 0; i < tableBody.children.length - 1; i++) {
        const row = Doc.parseTemplate(tableBody.children[i] as HTMLElement)
        if (running) {
          Doc.hide(row.upBtn, row.downBtn)
        } else {
          Doc.setVis(i !== 0, row.upBtn)
          Doc.setVis(i !== tableBody.children.length - 2, row.downBtn)
        }
      }
    }

    Doc.hide(errElement)
    const setErr = (err: string) => {
      errElement.textContent = err
      Doc.show(errElement)
    }
    let lots = 0
    // For percentages, actual gap factor is like 0.01, but displayed is 1%
    let actualGapFactor = 0
    let displayedGapFactor = 0
    const gapStrategy = this.updatedConfig.marketMakingConfig.gapStrategy
    const unit = this.gapFactorHeaderUnitRange(gapStrategy)[1]
    if (initialLoadPlacement) {
      lots = initialLoadPlacement.lots
      actualGapFactor = initialLoadPlacement.gapFactor
      displayedGapFactor = this.convertGapFactor(actualGapFactor, gapStrategy, true)
    } else {
      lots = parseInt(lotsElement.value || '0')
      displayedGapFactor = parseFloat(gapFactorElement.value || '0')
      actualGapFactor = this.convertGapFactor(displayedGapFactor, gapStrategy, false)
      if (lots === 0) {
        setErr('Lots must be greater than 0')
        return
      }
      const gapFactorErr = this.checkGapFactorRange(gapStrategy, displayedGapFactor)
      if (gapFactorErr) {
        setErr(gapFactorErr)
        return
      }

      const placements = getPlacementsList(isBuy)
      if (
        placements.find(
          (placement) =>
            placement.lots === lots && placement.gapFactor === actualGapFactor
        )
      ) {
        setErr('Placement already exists')
        return
      }

      placements.push({ lots, gapFactor: actualGapFactor })
    }

    const newRow = page.placementRowTmpl.cloneNode(true) as PageElement
    const newRowTmpl = Doc.parseTemplate(newRow)
    newRowTmpl.priority.textContent = `${tableBody.children.length}`
    newRowTmpl.lots.textContent = `${lots}`
    newRowTmpl.gapFactor.textContent = `${displayedGapFactor} ${unit}`
    newRowTmpl.removeBtn.onclick = () => {
      const placements = getPlacementsList(isBuy)
      const index = placements.findIndex((placement) => {
        return placement.lots === lots && placement.gapFactor === actualGapFactor
      })
      if (index === -1) return
      placements.splice(index, 1)
      newRow.remove()
      updateArrowVis()
      this.updateModifiedMarkers()
    }
    if (running) {
      Doc.hide(newRowTmpl.removeBtn)
    }

    newRowTmpl.upBtn.onclick = () => {
      const placements = getPlacementsList(isBuy)
      const index = placements.findIndex(
        (placement) =>
          placement.lots === lots && placement.gapFactor === actualGapFactor
      )
      if (index === 0) return
      const prevPlacement = placements[index - 1]
      placements[index - 1] = placements[index]
      placements[index] = prevPlacement
      newRowTmpl.priority.textContent = `${index}`
      newRow.remove()
      tableBody.insertBefore(newRow, tableBody.children[index - 1])
      const movedDownTmpl = Doc.parseTemplate(
        tableBody.children[index] as HTMLElement
      )
      movedDownTmpl.priority.textContent = `${index + 1}`
      updateArrowVis()
      this.updateModifiedMarkers()
    }
    newRowTmpl.downBtn.onclick = () => {
      const placements = getPlacementsList(isBuy)
      const index = placements.findIndex(
        (placement) =>
          placement.lots === lots && placement.gapFactor === actualGapFactor
      )
      if (index === placements.length - 1) return
      const nextPlacement = placements[index + 1]
      placements[index + 1] = placements[index]
      placements[index] = nextPlacement
      newRowTmpl.priority.textContent = `${index + 2}`
      newRow.remove()
      tableBody.insertBefore(newRow, tableBody.children[index + 1])
      const movedUpTmpl = Doc.parseTemplate(
        tableBody.children[index] as HTMLElement
      )
      movedUpTmpl.priority.textContent = `${index + 1}`
      updateArrowVis()
      this.updateModifiedMarkers()
    }
    tableBody.insertBefore(newRow, addPlacementRow)
    updateArrowVis()
  }

  setGapFactorLabels (gapStrategy: string) {
    const page = this.page
    const header = this.gapFactorHeaderUnitRange(gapStrategy)[0]
    page.buyGapFactorHdr.textContent = header
    page.sellGapFactorHdr.textContent = header
  }

  setupSettings (running: boolean) {
    const page = this.page

    // Gap Strategy
    page.gapStrategySelect.onchange = () => {
      if (!page.gapStrategySelect.value) return
      this.updatedConfig.marketMakingConfig.gapStrategy = page.gapStrategySelect.value
      while (page.buyPlacementsTableBody.children.length > 1) {
        page.buyPlacementsTableBody.children[0].remove()
      }
      while (page.sellPlacementsTableBody.children.length > 1) {
        page.sellPlacementsTableBody.children[0].remove()
      }
      this.updatedConfig.marketMakingConfig.buyPlacements = []
      this.updatedConfig.marketMakingConfig.sellPlacements = []
      this.setGapFactorLabels(page.gapStrategySelect.value)
      this.updateModifiedMarkers()
    }
    if (running) {
      page.gapStrategySelect.setAttribute('disabled', 'true')
    }

    // Buy/Sell Placements
    page.addBuyPlacementBtn.onclick = () => {
      this.addPlacement(true, null, false)
      page.addBuyPlacementLots.value = ''
      page.addBuyPlacementGapFactor.value = ''
      this.updateModifiedMarkers()
    }
    page.addSellPlacementBtn.onclick = () => {
      this.addPlacement(false, null, false)
      page.addSellPlacementLots.value = ''
      page.addSellPlacementGapFactor.value = ''
      this.updateModifiedMarkers()
    }
    Doc.setVis(!running, page.addBuyPlacementRow, page.addSellPlacementRow)

    // Drift Tolerance
    const updatedDriftTolerance = (x: number) => {
      this.updatedConfig.marketMakingConfig.driftTolerance = x
    }
    const changed = () => {
      this.updateModifiedMarkers()
    }
    const doNothing = () => {
      /* do nothing */
    }
    const currDriftTolerance = this.updatedConfig.marketMakingConfig.driftTolerance
    this.driftToleranceRangeHandler = new XYRangeHandler(
      driftToleranceRange,
      currDriftTolerance,
      updatedDriftTolerance,
      changed,
      doNothing,
      false,
      false,
      running
    )
    page.driftToleranceContainer.appendChild(
      this.driftToleranceRangeHandler.control
    )

    page.useOracleCheckbox.onchange = () => {
      if (page.useOracleCheckbox.checked) {
        Doc.show(page.oracleBiasSection, page.oracleWeightingSection)
        this.updatedConfig.marketMakingConfig.oracleWeighting = defaultMarketMakingConfig.oracleWeighting
        this.updatedConfig.marketMakingConfig.oracleBias = defaultMarketMakingConfig.oracleBias
        this.oracleWeightingRangeHandler.setValue(defaultMarketMakingConfig.oracleWeighting)
        this.oracleBiasRangeHandler.setValue(defaultMarketMakingConfig.oracleBias)
      } else {
        Doc.hide(page.oracleBiasSection, page.oracleWeightingSection)
        this.updatedConfig.marketMakingConfig.oracleWeighting = 0
      }
    }
    if (running) {
      page.useOracleCheckbox.setAttribute('disabled', 'true')
    }

    // Oracle Bias
    const currOracleBias = this.originalConfig.marketMakingConfig.oracleBias
    const updatedOracleBias = (x: number) => {
      this.updatedConfig.marketMakingConfig.oracleBias = x
    }
    this.oracleBiasRangeHandler = new XYRangeHandler(
      oracleBiasRange,
      currOracleBias,
      updatedOracleBias,
      changed,
      doNothing,
      false,
      false,
      running
    )
    page.oracleBiasContainer.appendChild(this.oracleBiasRangeHandler.control)

    // Oracle Weighting
    const currOracleWeighting = this.originalConfig.marketMakingConfig.oracleWeighting
    const updatedOracleWeighting = (x: number) => {
      this.updatedConfig.marketMakingConfig.oracleWeighting = x
    }
    this.oracleWeightingRangeHandler = new XYRangeHandler(
      oracleWeightRange,
      currOracleWeighting,
      updatedOracleWeighting,
      changed,
      doNothing,
      false,
      false,
      running
    )
    page.oracleWeightingContainer.appendChild(
      this.oracleWeightingRangeHandler.control
    )

    // Empty Market Rate
    page.emptyMarketRateCheckbox.onchange = () => {
      if (page.emptyMarketRateCheckbox.checked) {
        this.updatedConfig.marketMakingConfig.emptyMarketRate = 0
        Doc.show(page.emptyMarketRateInput)
        this.updateModifiedMarkers()
      } else {
        delete this.updatedConfig.marketMakingConfig.emptyMarketRate
        Doc.hide(page.emptyMarketRateInput)
        this.updateModifiedMarkers()
      }
    }
    page.emptyMarketRateInput.onchange = () => {
      const emptyMarketRate = parseFloat(
        page.emptyMarketRateInput.value || '0'
      )
      console.log('emptyMarketRate', emptyMarketRate)
      this.updatedConfig.marketMakingConfig.emptyMarketRate = emptyMarketRate
      this.updateModifiedMarkers()
    }
    if (running) {
      page.emptyMarketRateCheckbox.setAttribute('disabled', 'true')
      page.emptyMarketRateInput.setAttribute('disabled', 'true')
    }

    // Split Tx Allowed
    page.splitTxCheckbox.onchange = () => {
      this.updatedConfig.marketMakingConfig.splitTxAllowed = !!page.splitTxCheckbox.checked
      Doc.setVis(
        this.updatedConfig.marketMakingConfig.splitTxAllowed,
        page.splitBufferSection
      )
      this.updateModifiedMarkers()
    }
    if (running) {
      page.splitTxCheckbox.setAttribute('disabled', 'true')
    }

    const currSplitBuffer = this.originalConfig.marketMakingConfig.splitBuffer
    const updatedSplitBuffer = (x: number) => {
      this.updatedConfig.marketMakingConfig.splitBuffer = Math.round(x)
      this.updateModifiedMarkers()
    }
    this.splitBufferRangeHandler = new XYRangeHandler(
      splitTxBufferRange,
      currSplitBuffer,
      updatedSplitBuffer,
      changed,
      doNothing,
      true,
      true,
      running
    )
    page.splitBufferContainer.appendChild(this.splitBufferRangeHandler.control)
  }

  setOriginalValues (running: boolean) {
    const page = this.page
    this.updatedConfig = JSON.parse(JSON.stringify(this.originalConfig))

    if (!page.gapStrategySelect.options) return
    Array.from(page.gapStrategySelect.options).forEach(
      (opt: HTMLOptionElement) => {
        if (opt.value === this.originalConfig.marketMakingConfig.gapStrategy) {
          opt.selected = true
        }
      }
    )
    this.setGapFactorLabels(this.originalConfig.marketMakingConfig.gapStrategy)

    while (page.buyPlacementsTableBody.children.length > 1) {
      page.buyPlacementsTableBody.children[0].remove()
    }
    while (page.sellPlacementsTableBody.children.length > 1) {
      page.sellPlacementsTableBody.children[0].remove()
    }
    this.originalConfig.marketMakingConfig.buyPlacements.forEach((placement) => {
      this.addPlacement(true, placement, running)
    })
    this.originalConfig.marketMakingConfig.sellPlacements.forEach((placement) => {
      this.addPlacement(false, placement, running)
    })

    page.emptyMarketRateCheckbox.checked =
    this.originalConfig.marketMakingConfig.emptyMarketRate !== undefined
    Doc.setVis(
      !!page.emptyMarketRateCheckbox.checked,
      page.emptyMarketRateInput
    )
    page.emptyMarketRateInput.value = `${
      this.originalConfig.marketMakingConfig.emptyMarketRate || 0
    }`

    page.splitTxCheckbox.checked = this.originalConfig.marketMakingConfig.splitTxAllowed
    Doc.setVis(
      this.originalConfig.marketMakingConfig.splitTxAllowed,
      page.splitBufferSection
    )

    if (this.originalConfig.marketMakingConfig.oracleWeighting === 0) {
      page.useOracleCheckbox.checked = false
      Doc.hide(page.oracleBiasSection, page.oracleWeightingSection)
    }

    this.oracleBiasRangeHandler.setValue(this.originalConfig.marketMakingConfig.oracleBias)
    this.oracleWeightingRangeHandler.setValue(this.originalConfig.marketMakingConfig.oracleWeighting)
    this.splitBufferRangeHandler.setValue(this.originalConfig.marketMakingConfig.splitBuffer)
    this.driftToleranceRangeHandler.setValue(this.originalConfig.marketMakingConfig.driftTolerance)

    this.updateModifiedMarkers()
  }

  async saveSettings () {
    await app().updateMarketMakingConfig(this.updatedConfig)
    this.originalConfig = JSON.parse(JSON.stringify(this.updatedConfig))
    this.updateModifiedMarkers()
  }

  setupBalanceSelectors (allConfigs: BotConfig[], running: boolean) {
    const page = this.page

    const baseAsset = app().assets[this.updatedConfig.baseAsset]
    const quoteAsset = app().assets[this.updatedConfig.quoteAsset]

    const availableBaseBalance = baseAsset.wallet.balance.available
    const availableQuoteBalance = quoteAsset.wallet.balance.available

    let baseReservedByOtherBots = 0
    let quoteReservedByOtherBots = 0
    allConfigs.forEach((market) => {
      // check if base, quote, and host of market is the same as this.updatedMarket
      if (market.baseAsset === this.updatedConfig.baseAsset && market.quoteAsset === this.updatedConfig.quoteAsset &&
          market.host === this.updatedConfig.host) {
        return
      }

      if (market.baseAsset === this.updatedConfig.baseAsset) {
        baseReservedByOtherBots += market.baseBalance
      }
      if (market.quoteAsset === this.updatedConfig.baseAsset) {
        baseReservedByOtherBots += market.quoteBalance
      }
      if (market.baseAsset === this.updatedConfig.quoteAsset) {
        quoteReservedByOtherBots += market.baseBalance
      }
      if (market.quoteAsset === this.updatedConfig.quoteAsset) {
        quoteReservedByOtherBots += market.quoteBalance
      }
    })

    let baseMaxPercent = 0
    let quoteMaxPercent = 0
    if (baseReservedByOtherBots < 100) {
      baseMaxPercent = 100 - baseReservedByOtherBots
    }
    if (quoteReservedByOtherBots < 100) {
      quoteMaxPercent = 100 - quoteReservedByOtherBots
    }

    const baseMaxAvailable = Doc.conventionalCoinValue(
      (availableBaseBalance * baseMaxPercent) / 100,
      baseAsset.unitInfo
    )
    const quoteMaxAvailable = Doc.conventionalCoinValue(
      (availableQuoteBalance * quoteMaxPercent) / 100,
      quoteAsset.unitInfo
    )

    const baseXYRange: XYRange = {
      start: {
        label: '0%',
        x: 0,
        y: 0
      },
      end: {
        label: `${baseMaxPercent}%`,
        x: baseMaxPercent,
        y: baseMaxAvailable
      },
      xUnit: '%',
      yUnit: baseAsset.symbol
    }

    const quoteXYRange: XYRange = {
      start: {
        label: '0%',
        x: 0,
        y: 0
      },
      end: {
        label: `${quoteMaxPercent}%`,
        x: quoteMaxPercent,
        y: quoteMaxAvailable
      },
      xUnit: '%',
      yUnit: quoteAsset.symbol
    }

    Doc.hide(
      page.noBaseBalance,
      page.noQuoteBalance,
      page.baseBalanceContainer,
      page.quoteBalanceContainer
    )
    Doc.empty(page.baseBalanceContainer, page.quoteBalanceContainer)
    const doNothing = () => {
      /* do nothing */
    }

    if (baseMaxAvailable > 0) {
      const updatedBase = (x: number) => {
        this.updatedConfig.baseBalance = x
        this.updateModifiedMarkers()
      }
      const currBase = this.originalConfig.baseBalance
      const baseRangeHandler = new XYRangeHandler(
        baseXYRange,
        currBase,
        updatedBase,
        doNothing,
        doNothing,
        false,
        true,
        running
      )
      page.baseBalanceContainer.appendChild(baseRangeHandler.control)
      Doc.show(page.baseBalanceContainer)
    } else {
      Doc.show(page.noBaseBalance)
    }

    if (quoteMaxAvailable > 0) {
      const updatedQuote = (x: number) => {
        this.updatedConfig.quoteBalance = x
        this.updateModifiedMarkers()
      }
      const currQuote = this.originalConfig.quoteBalance
      const quoteRangeHandler = new XYRangeHandler(
        quoteXYRange,
        currQuote,
        updatedQuote,
        doNothing,
        doNothing,
        false,
        true,
        running
      )
      Doc.empty(page.quoteBalanceContainer)
      page.quoteBalanceContainer.appendChild(quoteRangeHandler.control)
      Doc.show(page.quoteBalanceContainer)
    } else {
      Doc.show(page.noQuoteBalance)
    }
  }

  async fetchOracles (): Promise<void> {
    const page = this.page
    const { baseAsset, quoteAsset } = this.originalConfig

    const res = await postJSON('/api/marketreport', { baseID: baseAsset, quoteID: quoteAsset })
    Doc.hide(page.oraclesLoading)

    if (!app().checkResponse(res)) {
      page.oraclesErrMsg.textContent = res.msg
      Doc.show(page.oraclesErr)
      return
    }

    const r = res.report as MarketReport
    if (!r.oracles || r.oracles.length === 0) {
      Doc.show(page.noOracles)
    } else {
      Doc.empty(page.oracles)
      for (const o of r.oracles ?? []) {
        const tr = page.oracleTmpl.cloneNode(true) as PageElement
        page.oracles.appendChild(tr)
        const tmpl = Doc.parseTemplate(tr)
        tmpl.logo.src = 'img/' + o.host + '.png'
        tmpl.host.textContent = ExchangeNames[o.host]
        tmpl.volume.textContent = Doc.formatFourSigFigs(o.usdVol)
        tmpl.price.textContent = Doc.formatFourSigFigs((o.bestBuy + o.bestSell) / 2)
      }
      page.avgPrice.textContent = r.price ? Doc.formatFourSigFigs(r.price) : '0'
      Doc.show(page.oraclesTable)
    }

    page.baseFiatRateSymbol.textContent = app().assets[baseAsset].symbol.toUpperCase()
    page.baseFiatRateLogo.src = Doc.logoPathFromID(baseAsset)
    if (r.baseFiatRate > 0) {
      page.baseFiatRate.textContent = Doc.formatFourSigFigs(r.baseFiatRate)
    } else {
      page.baseFiatRate.textContent = 'N/A'
    }

    page.quoteFiatRateSymbol.textContent = app().assets[quoteAsset].symbol.toUpperCase()
    page.quoteFiatRateLogo.src = Doc.logoPathFromID(quoteAsset)
    if (r.quoteFiatRate > 0) {
      page.quoteFiatRate.textContent = Doc.formatFourSigFigs(r.quoteFiatRate)
    } else {
      page.quoteFiatRate.textContent = 'N/A'
    }
    Doc.show(page.fiatRates)
  }
}

const ExchangeNames: Record<string, string> = {
  'binance.com': 'Binance',
  'coinbase.com': 'Coinbase',
  'bittrex.com': 'Bittrex',
  'hitbtc.com': 'HitBTC',
  'exmo.com': 'EXMO'
}
