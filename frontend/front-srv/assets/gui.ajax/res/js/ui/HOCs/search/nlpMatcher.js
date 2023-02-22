/*
 * Copyright 2023 Charles du Jeu - Abstrium SAS <team (at) pyd.io>
 * This file is part of Pydio.
 *
 * Pydio is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */
import nlpLoader from './nlpLoader'

class nlpMatches {
    constructor(matches, remaining) {
        this.__matches = matches
        this.__remaining = remaining
    }

    getMatches(){
        return this.__matches
    }

    getRemaining() {
        return this.__remaining;
    }

    asValues() {
        return this.__matches.reduce((object, value) => {
            if(value.key === '#remaining#') {
                object['basenameOrContent'] = value.value;
            } else if(value.key === 'after' || value.key === 'before') {
                if(!object['ajxp_modiftime']) {
                    object['ajxp_modiftime'] = {}
                }
                object['ajxp_modiftime'][value.key === 'after'?'from':'to'] = value.value
            } else {
                object[value.key] = value.value;
            }
            return object;
        } , {basenameOrContent: ''}) // Clear by default
    }
}

const match = (text, getSearchOptions) => {
    return Promise.all([nlpLoader(), getSearchOptions()]).then(([lib, searchOptions]) => {

        const {nlp, nlpLanguage, extensions} = lib;
        const {indexedMeta=[]} = searchOptions
        const entities = {}
        const removes = [];

        const doc = nlp(text.toLowerCase())
        const dates = doc.dates()
        const dd = dates.get(0)
        if(dd && (dd.start || dd.end)) {
            if(dd.start){
                entities['after'] = {label:'After', value: new Date(dd.start), isDate:true}
            }
            if(dd.end){
                entities['before'] = {label: 'Before', value: new Date(dd.end), isDate:true}
            }
            removes.push(dates)
        }

        doc.compute('root')

        let Terms = {
            'file': 'file',
            'folder': 'folder'
        }
        if(nlpLanguage === 'fr') {
            Terms = {
                'file': 'fichier',
                'folder': 'répertoire'
            }
        }


        const files = doc.match(extensions+'? {'+Terms.file+'}?')
        const folders = doc.match('{'+Terms.folder+'}')
        if(files && files.text()) {
            entities['ajxp_mime'] = {value:'ajxp_file', label:'Files Only', labelOnly: true}
            const exts = files.match( '[<ext>' + extensions+'?] {file}?', 'ext')
            if (exts.text()){
                entities['ajxp_mime'] = {value:exts.text('root'), label:'Extension'}
            }
            removes.push(files)
        } else if(folders && folders.text()) {
            entities['ajxp_mime'] = {value:'ajxp_folder', label:'Folders Only', labelOnly: true};
            removes.push(folders)
        }

        if(indexedMeta){
            let knownValues = {}
            indexedMeta.forEach((meta) => {
                const {label, type, ns, data} = meta
                const metaDoc = nlp(label)
                metaDoc.compute('root')
                const searchLabel = metaDoc.text('root')
                let searchNLPTags, isNumber, presetValues;
                switch (type){
                    case 'integer':
                    case 'stars_rate':
                        searchNLPTags = '#Value|#Adverb'
                        isNumber = true
                        break;
                    case 'choice':
                        presetValues = data.items
                        break
                    case 'css_label':
                        presetValues = [{value:'todo'},{value:'work'},{value:'important'},{value:'personal'}, {value:'low'}]
                        break
                    default:
                        searchNLPTags = '#Noun|#Adjective'
                        break;
                }
                if(presetValues) {

                    // try to find items value directly
                    const searchMatch = doc.match('('+presetValues.map(i => '~'+i.value+'~').join('|')+')')
                    const search = searchMatch.text()
                    if(search && !knownValues[search]) {
                        entities['ajxp_meta_' + ns] = {value:search, label, meta}
                        knownValues[search] = search
                        removes.push(searchMatch)
                    }
                } else if (searchNLPTags) {

                    const searchAll = doc.match('{'+searchLabel+'} #Preposition? ('+searchNLPTags+')')
                    if(searchAll.text()) {
                        const searchT = doc.match('{'+searchLabel+'} #Preposition? [<tag>('+searchNLPTags+')]', 'tag')
                        let tags = searchT.text()
                        if(tags && isNumber){
                            tags = searchT.numbers().toCardinal().toNumber().text()
                        }
                        if(tags && !knownValues[tags]) {
                            entities['ajxp_meta_' + ns] = {value:tags,label, meta}
                            knownValues[tags] = tags
                            removes.push(searchAll)
                        }
                    }
                }

            })
        }

        const matches = Object.keys(entities).map(k => {return {key:k, ...entities[k]}});

        removes.forEach(m => m.remove())
        doc.match('(#Verb|#Preposition|#Adverb)').remove()
        const remaining = doc.text('trim')
        return new nlpMatches(matches, remaining);

    }).catch(e => {

        console.debug('Ignoring NLP loading ', e)

    })
}

export default match