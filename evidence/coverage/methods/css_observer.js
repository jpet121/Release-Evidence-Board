/* Verification-only CSSOM instrumentation; never embedded in the application. */
return (async () => {
  const sources = arguments[0];
  if (window.rebCSS) {
    if(window.rebCSS.read().sources.some(hash=>!sources.some(s=>s.sha256===hash))) throw Error('unmapped active CSS stylesheet');
    return window.rebCSS.read();
  }
  const seen = [], observations = [], errors = [];
  const normalize = s => s.replace(/\r\n?/g, '\n');
  const prefix = '--reb-application-';
  const all = [...document.querySelectorAll('*')];
  const signature = () => JSON.stringify(all.map(e => {
    const s = getComputedStyle(e), box = e.getBoundingClientRect();
    return [[...s].filter(p => !p.startsWith(prefix)).sort().map(p => [p,s.getPropertyValue(p)]),
      [box.x,box.y,box.width,box.height]];
  }));
  // Firefox can resolve a hidden details descendant's grid track serialization
  // lazily on its first computed-style read. Stabilize the *uninstrumented*
  // document first, then require a repeatable baseline before adding probes.
  signature();
  const before = signature();
  if(before!==signature()) throw Error('uninstrumented CSS presentation is not stable');
  for (const sheet of document.styleSheets) {
    if(sheet.disabled || sheet.media.mediaText) throw Error('unsupported inactive or sheet-level media source');
    // The Python caller verifies external response bytes. Application CSP
    // intentionally prohibits script fetch; do not weaken it for measurement.
    const source = sources.find(s => sheet.href ? s.href === sheet.href : normalize(s.text) === normalize(sheet.ownerNode.textContent));
    if (!source) throw Error('unmapped active CSS stylesheet');
    const raw = normalize(source.text);
    if (seen.some(s => s.source.sha256 === source.sha256)) throw Error('duplicate active stylesheet');
    if (raw.includes(prefix) || all.some(e => e.getAttribute('style')?.includes(prefix))) throw Error('marker collision');
    const reference = new CSSStyleSheet();
    reference.replaceSync(raw);
    const flatten = (list, path=[]) => [...list].flatMap((r,i) => [{rule:r,index:[...path,i]},
      ...(r.type === CSSRule.MEDIA_RULE ? flatten(r.cssRules,[...path,i]) : [])]);
    const native = flatten(sheet.cssRules), expected = flatten(reference.cssRules);
    if (native.length !== source.rules.length || native.length !== expected.length) throw Error('dropped or unmapped CSS rule');
    const mapped = source.rules.map((r,i) => {
      const actual = native[i].rule;
      if (JSON.stringify(native[i].index) !== JSON.stringify(r.index) || actual.cssText !== expected[i].rule.cssText) throw Error('CSSOM/source mismatch');
      if (r.kind === 'media') {
        if (actual.type !== CSSRule.MEDIA_RULE) throw Error('CSS group type mismatch');
      } else {
        if (actual.type !== CSSRule.STYLE_RULE) throw Error('CSS style type mismatch');
        for (const [name,value] of r.declarations) {
          const declaration = document.createElement('span').style;
          declaration.setProperty(name,value.replace(/\s*!important\s*$/i,''), /!important\s*$/i.test(value) ? 'important' : '');
          if (!declaration.getPropertyValue(name)) throw Error('invalid authored CSS declaration: '+name+':'+value);
        }
        // Registered custom properties cannot inherit a parent's marker hit.
        const marker = prefix+source.sha256.slice(0,16)+'-'+r.id;
        if (all.some(e => getComputedStyle(e).getPropertyValue(marker).trim())) throw Error('computed marker collision');
        CSS.registerProperty({name:marker,syntax:'<integer>',inherits:false,initialValue:'0'});
        actual.style.setProperty(marker,String(r.id+1));
        return {...r,actual,marker};
      }
      return {...r,actual};
    });
    seen.push({source,mapped,sheet,cssText:[...sheet.cssRules].map(r=>r.cssText).join('\n')});
  }
  if (!seen.length) throw Error('no active CSS source');
  const after=signature();
  if (before !== after) {
    const a=JSON.parse(before),b=JSON.parse(after),i=a.findIndex((v,i)=>JSON.stringify(v)!==JSON.stringify(b[i]));
    const properties=a[i][0].filter(v=>!b[i][0].some(w=>JSON.stringify(v)===JSON.stringify(w)));
    throw Error('CSS probes changed computed presentation or geometry: '+JSON.stringify({url:location.href,tag:all[i].tagName,properties,before:a[i][1],after:b[i][1]}));
  }
  function scan(event) {
    try {
      if(document.styleSheets.length!==seen.length) throw Error('active stylesheet inventory changed');
      for (const {source,mapped,sheet,cssText} of seen) {
        if(sheet.disabled || sheet.media.mediaText || ![...document.styleSheets].includes(sheet) ||
           [...sheet.cssRules].map(r=>r.cssText).join('\n')!==cssText) throw Error('active stylesheet changed after binding');
        const values = new Map(mapped.filter(r => r.kind === 'media').map(r => [r.id,matchMedia(r.actual.conditionText).matches]));
        const outcomes = mapped.filter(r => r.kind==='media' && r.parents.every(p=>values.get(p))).map(r=>[r.id,values.get(r.id)]);
        const hits = mapped.filter(r => r.kind === 'style' && r.parents.every(p => values.get(p)) &&
          [...document.querySelectorAll(r.actual.selectorText)].some(e =>
            e.matches(r.actual.selectorText) && getComputedStyle(e).getPropertyValue(r.marker).trim() === String(r.id+1))).map(r=>r.id);
        observations.push({source:source.sha256,rules:hits,outcomes,event,url:location.href,width:innerWidth,print:matchMedia('print').matches});
      }
    } catch (e) { errors.push(String(e)); }
  }
  for (const condition of new Set(seen.flatMap(s => s.mapped.filter(r=>r.kind==='media').map(r=>r.actual.conditionText))))
    matchMedia(condition).addEventListener('change',()=>scan('native-media-change'));
  window.rebCSS = {scan,read:()=>({observations,errors,presentationUnchanged:true,sources:seen.map(s=>s.source.sha256)})};
  scan('installed');
  return window.rebCSS.read();
})().then(arguments[arguments.length-1], e=>arguments[arguments.length-1]({error:String(e)}));
